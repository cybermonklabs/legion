package docs

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// A user_threads row and a comment_deliveries attempt row the release before this one moves after
// the backfill are marked again by this build's block-index ticks, with no second backfill, and the
// census reads no unmarked row of either kind.
//
// That release's writes of those rows state nothing in the store, and each update of a row this
// build marked takes the mark off it (migrations 20261116150000 and 20261116160000), so the row is
// unmarked until the backfill takes it again; and R9 says of the unmarked class: "That class
// reaches zero when the backfill ends ... No class is permanent." Its read mark moves a person's
// user_threads row and appends a user_thread_state.updated naming the person and no thread; its
// sender settles an attempt and appends the attempt's comment.delivery receipt, and claims one with
// no event at all. Each tick that owes the object pass marks one batch of each kind again, through
// the backfill's own passes (graphcensus.CatchUpOwedRows), and a tick owes it on such an event, as
// well as on every tick while another build serves (graphcensus.EventsOweObjectPass,
// readEventsAfterCursor).
//
// The test seeds alice's comment on CORE-1, which begins her row of its thread, with two pending
// attempts to reach two sessions, and runs the backfill. Then a process of that release takes every
// period of the lease, so no tick of this build runs while it serves: it moves alice's read mark on
// the thread as its read route does, and settles the first attempt as its sender does, each with
// its event; it leaves, and this build's next tick must mark both rows again and give the session
// the settled attempt reached its node, which the backfill gave none while the attempt was pending
// (graphcensus.CatchUpEvents). Then a process of that release serves beside this build and claims
// the second attempt, which appends no event; this build's tick while it serves must mark that row
// again. Its control is the catch-up taken out of writeObjectNodesAnotherBuildWrote: the rows stay
// unmarked after each tick.
func TestAThreadMarkAndAnAttemptThePreviousReleaseMoveAfterTheBackfillAreMarkedAgainByTheTick(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "inbox-rows"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	const settled, claimed = "previous-release-settled-session", "previous-release-claimed-session"
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'A remark written before the store existed.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("seed alice's comment: %v", err)
	}
	for _, seed := range []struct {
		what, statement string
		args            []any
	}{
		// Rows of every other kind the census reads, so the census can pass only on agreement.
		{"a session's comment", `insert into comments (issue_key, author, body)
			values ('CORE-1', '{"kind":"session","id":"previous-release-inbox-session"}', 'A session''s remark written before the store existed.')`, nil},
		{"the route", `update issues set route = 'role:previous-release-inbox-role' where key = 'CORE-1'`, nil},
		{"the two attempts", `insert into comment_deliveries (comment_id, target, attempt, delivery, state, session_id, claimed_at)
			values ($1::uuid, $2, 1, 'aside', 'pending', $3, now()), ($1::uuid, $4, 1, 'aside', 'pending', $5, null)`,
			[]any{comment, "session:" + settled, settled, "session:" + claimed, claimed}},
	} {
		if _, err := database.Pool.Exec(ctx, seed.statement, seed.args...); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")
	requireInboxRowsMarked(t, database, 0, 0, "after the backfill")

	// A process of the release before takes every period of the lease, moves alice's read mark and
	// settles the first attempt, and leaves.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	previous := previousReleaseConn(t, database)
	markThreadReadAsThePreviousRelease(t, previous, "alice", comment)
	settleAttemptAsThePreviousRelease(t, previous, comment, "session:"+settled, settled)
	requireInboxRowsMarked(t, database, 1, 1, "after the previous release moved them, before this build's tick")
	previousLeaves()
	alone := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), marked %d inbox row(s), another build %t",
		alone.Events, alone.InboxRows, alone.AnotherBuild)
	if alone.InboxRows != 3 {
		t.Errorf("the first tick after the previous release left marked %d inbox row(s), want alice's thread row, the settled attempt and the session it reached", alone.InboxRows)
	}
	requireInboxRowsMarked(t, database, 0, 0, "after the first tick after the previous release left, with no second backfill")
	requireCensusReadsZero(t, database, "after the previous release moved a thread row and settled an attempt, with no second backfill")

	// A process of that release serves beside this build and claims the second attempt, which
	// appends no event.
	previousLeaves = registerProcess(t, database, previousReleaseCommit)
	claimAttemptAsThePreviousRelease(t, previous, comment, "session:"+claimed)
	requireInboxRowsMarked(t, database, 0, 1, "after the previous release claimed an attempt, before this build's tick")
	roll := awaitedBlockIndexTick(t, service)
	t.Logf("the tick while the previous release served marked %d inbox row(s), another build %t", roll.InboxRows, roll.AnotherBuild)
	if !roll.AnotherBuild || roll.InboxRows != 1 {
		t.Errorf("the tick while the previous release served marked %d inbox row(s) and found another build %t, want the claimed attempt and another build",
			roll.InboxRows, roll.AnotherBuild)
	}
	requireInboxRowsMarked(t, database, 0, 0, "after a tick while the previous release served, with no second backfill")
	previousLeaves()
	awaitedBlockIndexTick(t, service)
	requireCensusReadsZero(t, database, "after the previous release claimed an attempt and left, with no second backfill")
}

// requireInboxRowsMarked fails the test unless the census counts threads unmarked user_threads rows
// and attempts unmarked comment_deliveries rows (graphcensus.KindReport.Unmarked).
func requireInboxRowsMarked(t *testing.T, database *store.Store, threads, attempts int64, when string) {
	t.Helper()
	census := takeStoreCensus(t, database)
	for _, want := range []struct {
		kind     string
		unmarked int64
	}{{"user_threads", threads}, {"comment_deliveries", attempts}} {
		found := false
		for _, report := range census.Kinds {
			if report.Kind != want.kind {
				continue
			}
			found = true
			if report.Unmarked != want.unmarked {
				t.Errorf("the census counts %d unmarked %s row(s) %s, want %d", report.Unmarked, want.kind, when, want.unmarked)
			}
		}
		if !found {
			t.Fatalf("the census reads no kind %s", want.kind)
		}
	}
}

// previousReleaseConn is a connection to the test's database opened as the release before this one
// opens its own: from the database's address alone, so it carries none of the startup parameters
// store.Open gives this release's pool, store.StoreMarkerWriterSetting among them, and an update it
// makes takes the mark off the row it moves.
func previousReleaseConn(t *testing.T, database *store.Store) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect as the previous release: %v", err)
	}
	testcleanup.Add(t, func() { _ = conn.Close(context.Background()) })
	return conn
}

// markThreadReadAsThePreviousRelease moves login's read mark on thread as the release before this
// one's read route does (main's inboxReadLockThreads and inboxReadMarkThreads), through conn, and
// appends in the same transaction the user_thread_state.updated it appends for the project of the
// row it moved, owned by that project and naming login and no thread.
func markThreadReadAsThePreviousRelease(t *testing.T, conn *pgx.Conn, login, thread string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	threads := []string{thread}
	if _, err := tx.Exec(ctx, `
		select 1 from user_threads
		where login = $1 and thread_id = any($2::uuid[])
		order by thread_id
		for no key update
	`, login, threads); err != nil {
		t.Fatalf("lock %s's row of thread %s: %v", login, thread, err)
	}
	var project string
	if err := tx.QueryRow(ctx, `
		update user_threads set read_through = least($2::text::timestamptz, now())
		where login = $1 and thread_id = any($3::uuid[])
		  and read_through < least($2::text::timestamptz, now())
		returning project_key
	`, login, time.Now().UTC().Format(time.RFC3339Nano), threads).Scan(&project); err != nil {
		t.Fatalf("move %s's read mark on thread %s: %v", login, thread, err)
	}
	actor, err := json.Marshal(model.Actor{Kind: "user", ID: login})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(model.UserThreadStateEventPayload{Login: login})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		with bumped as (update projects set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (project_key, seq, type, actor, payload, notify)
		select $1, bumped.last_seq, 'user_thread_state.updated', $2, $3, false
		from bumped
	`, project, actor, payload); err != nil {
		t.Fatalf("append the user_thread_state.updated of %s: %v", login, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// previousReleaseSender is who the release before this one's mention sender appends a receipt as.
var previousReleaseSender = model.Actor{Kind: "system", ID: "previous-release-mention-sender"}

// settleAttemptAsThePreviousRelease settles the first attempt of comment to target sent, as the
// release before this one's sender settles an attempt it claimed (main's recordCommentDelivery
// statement), through conn, and appends in the same transaction the attempt's comment.delivery
// receipt (model.CommentDeliveryEventPayload) under the comment's issue, CORE-1.
func settleAttemptAsThePreviousRelease(t *testing.T, conn *pgx.Conn, comment, target, session string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var claimedAt time.Time
	if err := tx.QueryRow(ctx, `
		select claimed_at from comment_deliveries where comment_id = $1 and target = $2 and attempt = 1
	`, comment, target).Scan(&claimedAt); err != nil {
		t.Fatalf("read the claim on the attempt of %s to %s: %v", comment, target, err)
	}
	const envelope = "previous-release-envelope"
	tag, err := tx.Exec(ctx, `
		update comment_deliveries set envelope_id = $5, state = $6, error = $7, duplicate = $8
		where comment_id = $1 and target = $2 and attempt = $3
		  and state = 'pending' and claimed_at = $4
	`, comment, target, 1, claimedAt, envelope, "sent", nil, false)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("settle the attempt of %s to %s: %v (%d row(s))", comment, target, err, tag.RowsAffected())
	}
	actor, err := json.Marshal(previousReleaseSender)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(model.CommentDeliveryEventPayload{
		CommentID: comment, Target: target, Attempt: 1, Delivery: "aside", SessionID: new(session), State: "sent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordIssueEvent(ctx, tx, "CORE-1", "comment.delivery", actor, payload); err != nil {
		t.Fatalf("append the comment.delivery receipt of %s to %s: %v", comment, target, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// claimAttemptAsThePreviousRelease claims the first attempt of comment to target as the release
// before this one's sender claims an owed attempt (main's claimQueuedMention), through conn, which
// appends no event.
func claimAttemptAsThePreviousRelease(t *testing.T, conn *pgx.Conn, comment, target string) {
	t.Helper()
	tag, err := conn.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and `+model.DeliveryClaimFreeSQL,
		comment, target, 1)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("claim the attempt of %s to %s: %v (%d row(s))", comment, target, err, tag.RowsAffected())
	}
}
