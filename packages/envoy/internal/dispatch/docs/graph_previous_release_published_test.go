package docs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The session and the role the previous release's outbox publishes this file's issue message to,
// and the session that writes this file's comments.
const (
	publishedSession = "previous-release-published-session"
	publishedRole    = "previous-release-published-role"
	commentedSession = "previous-release-commenting-session"
)

// seedBesideTheStore writes, as rows written before the store existed, a comment of alice's and a
// session's on CORE-1 and the issue's route to publishedRole, and backfills the store, so the
// census can pass only on agreement, and fails unless it then reads zero with nothing owed.
func seedBesideTheStore(t *testing.T, database *store.Store, service *Service, slug string) {
	t.Helper()
	ctx := context.Background()
	appendIssueEvent(t, database)
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-published-author"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:`+publishedRole+`' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")
	requireNoInboxRowOwed(t, database, "after the backfill")
}

// requireCensusKindAgrees fails unless the census checks rows of kind with no difference and none
// unmarked.
func requireCensusKindAgrees(t *testing.T, database *store.Store, kind, when string) {
	t.Helper()
	for _, read := range takeStoreCensus(t, database).Kinds {
		if read.Kind != kind {
			continue
		}
		if read.Rows == 0 || read.RowDifferences != 0 || read.NodeDifferences != 0 || read.Unmarked != 0 {
			t.Errorf("the census checks %d row(s) of %s, reads %d row(s) no tuple states and %d tuple(s) no row projects (%v %v), and counts %d unmarked %s, want rows, no difference and none unmarked",
				read.Rows, kind, read.RowDifferences, read.NodeDifferences, read.ReportedRows, read.ReportedNodes, read.Unmarked, when)
		}
		return
	}
	t.Fatalf("the census reads no kind %s", kind)
}

// What the outbox of the release before this one records of an event it publishes after this
// build's block-index tick has read the event reaches the store at this build's next tick, with no
// second backfill, and the census reads no published destination no tuple states and no row of the
// queue: R9 says of the unmarked class "That class reaches zero when the backfill ends ... No class
// is permanent."
//
// That release's outbox adds the topics it published an event to to the event's
// published_destinations after the event commits, appends no event, and states no delivered_to of
// a message's event at all (the store's own release states them for a comment's event alone;
// outbox's deliveredItem there). The tick reads each event once, after its cursor, so by the time
// such a record lands it has read the event already and would never look at it again. The trigger
// of migration 20261122100122 queues the event as that record lands (events_published_owed), and
// the tick's catch-up states each queued event's delivered_to tuples as this release's outbox
// states them and takes the event off the queue (graphcensus.CatchUpOwedRows).
//
// A process of that release joins and writes an untargeted message on CORE-1, which is routed to a
// role, with its message.created. This build's tick reads that event while it serves, gives the
// message its node and states it. Only then does that release's outbox record the event as
// published to a session's topic and to the route's role's topic. The event is queued and its two
// destinations owe the store their delivered_to. That process leaves, and this build's next tick
// must leave the queue empty, the two tuples stated under the event, the census checking
// published_destinations with no difference and none unmarked, and every pass owing nothing. Its
// control is the trigger dropped, or the tick's read of the queue taken out of its catch-up
// (rollPasses): the two destinations stay owed and the census reads them as rows no tuple states.
func TestWhatThePreviousReleasesOutboxRecordsAfterTheTickReadTheEventReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	seedBesideTheStore(t, database, service, "published-beside")

	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	previous := previousReleaseConn(t, database)
	alice := model.Actor{Kind: "user", ID: "alice"}
	var message string
	var event int64
	inPreviousRelease(t, previous, "an untargeted issue message", func(ctx context.Context, tx pgx.Tx) {
		requireExec(t, tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body) values ('CORE-1', $1, 'For whoever the issue is routed to.')
			returning id::text
		`, actorBytes(t, alice)).Scan(&message), "write an untargeted issue message")
		event = recordInPreviousRelease(t, tx, "message.created", alice, map[string]any{"id": message})
	})
	read := awaitedBlockIndexTick(t, service)
	if cursor := blockIndexEventCursor(t, database, ""); cursor < event {
		t.Fatalf("this build's tick moved its cursor to %d, short of the message's event %d, so the record below would not land after the tick read the event", cursor, event)
	}
	t.Logf("the tick while the previous release served read %d event(s), gave %d object(s) their nodes, marked %d inbox row(s), another build %t",
		read.Events, read.ObjectNodes, read.InboxRows, read.AnotherBuild)

	// That release's outbox records what it published the event to, after the tick read it.
	reached := []string{contracts.AgentTopicPrefix + publishedSession, contracts.RoleTopicPrefix + publishedRole}
	inPreviousRelease(t, previous, "the outbox's record of the message's event", func(ctx context.Context, tx pgx.Tx) {
		execInPreviousRelease(t, tx, "the outbox's record of the message's event", `
			update events set published_destinations = published_destinations || $2::text[], published_at = coalesce(published_at, now())
			where id = $1
		`, event, reached)
	})
	owed := []inboxClass{{
		name: "the queue's row of the message's event",
		owed: `select count(*) from events_published_owed where event_id = $1`,
		args: []any{event},
	}, {
		name: "the destinations of the message's event no delivered_to states",
		owed: `select cardinality($3::text[]) - count(*) from graph_tuples
			where kind = $4 and from_kind = $5 and from_id = $2 and source_event = $1 and asserted`,
		args: []any{event, message, reached, graphstore.TupleDeliveredTo, graphstore.KindMessage},
	}}
	requireInboxClassesOwed(t, database, owed, true, "after the previous release's outbox recorded the message's event, before this build's next tick")
	previousLeaves()

	alone := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), marked %d inbox row(s), another build %t",
		alone.Events, alone.InboxRows, alone.AnotherBuild)
	requireInboxClassesOwed(t, database, owed, false, "after the first tick after the previous release left, with no second backfill")
	requireNoInboxRowOwed(t, database, "after the first tick after the previous release left")
	requireCensusKindAgrees(t, database, "published_destinations", "after the first tick after the previous release left")
	requireCensusReadsZero(t, database, "after the previous release's outbox recorded an event the tick had read and left, with no second backfill")
}

// A session's comment the store's own release writes after the backfill, with the comment's node,
// reaches the store at this build's next block-index tick, with no second backfill, and the census
// reads no comment_pointers row unmarked.
//
// That release writes a comment's node before its row, names it on the row and states the
// comment's author under its comment.created (api's commentStored there), but holds no
// tuples_stored: the comment's pointers and what it owed its thread are this release's. The tick's
// catch-up of the comments its events name reads only a comment whose row names no node
// (readEventPage), and a session's comment moves no user_threads row, so no thread pass reaches it
// either; the pointer pass and the pass past the store's bound run in the tick's catch-up of the
// rows another build left (graphcensus.CatchUpOwedRows), and the comment's event makes the tick owe
// it (graphcensus.EventsOweObjectPass).
//
// That release writes two such comments while it holds every period of the lease: one replying to
// the seeded session's comment, a thread no person takes part in, so its replies_to and part_of
// are rows of comment_pointers and no user_threads row moves; and one answering a block at an
// address past the store's bound, which no pointer pass takes and whose threads_recorded the pass
// past the bound sets. Once it leaves, this build's next tick must leave both marked, the census
// checking comment_pointers with no difference and none unmarked, and every pass owing nothing.
// Its control is the two passes taken out of the catch-up (rollPasses): both comments stay owed and
// comment_pointers reads one unmarked.
func TestASessionsCommentThePreviousReleaseWritesWithItsNodeAfterTheBackfillReachesTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	seedBesideTheStore(t, database, service, "commented-beside")
	var root string
	if err := database.Pool.QueryRow(context.Background(), `
		select id::text from comments where author->>'id' = 'previous-release-published-author'
	`).Scan(&root); err != nil {
		t.Fatalf("read the seeded session's comment: %v", err)
	}

	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	previous := previousReleaseConn(t, database)
	pastBound := "comment/" + strings.Repeat("b", graphstore.MaxAddressBytes) + "#b1"
	plain := writeCommentWithItsNodeAsTheStoresRelease(t, previous, "A session's reply written beside this build.", root, "")
	answering := writeCommentWithItsNodeAsTheStoresRelease(t, previous, "A session's answer to a block past the bound.", "", pastBound)
	owed := []inboxClass{{
		name: "the comment_pointers row of the session's comment",
		owed: `select count(*) from comments where id = $1::uuid and not tuples_stored`,
		args: []any{plain},
	}, {
		name: "the session's comment past the store's bound the old trigger still writes for",
		owed: `select count(*) from comments where id = $1::uuid and not tuples_stored and not threads_recorded`,
		args: []any{answering},
	}}
	requireInboxClassesOwed(t, database, owed, true, "after the previous release wrote the session's comments, before this build's tick")
	previousLeaves()

	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), gave %d comment(s) their nodes, marked %d inbox row(s), another build %t",
		tick.Events, tick.Comments, tick.InboxRows, tick.AnotherBuild)
	requireInboxClassesOwed(t, database, owed, false, "after the first tick after the previous release left, with no second backfill")
	requireNoInboxRowOwed(t, database, "after the first tick after the previous release left")
	requireCensusKindAgrees(t, database, "comment_pointers", "after the first tick after the previous release left")
	requireCensusReadsZero(t, database, "after the previous release wrote a session's comments with their nodes and left, with no second backfill")
}

// writeCommentWithItsNodeAsTheStoresRelease has the stand-in for the store's own release write,
// through conn in one transaction, a comment by commentedSession on CORE-1 as that release's API
// writes one: its comment.created, whose payload names the comment, the comment's node and its
// author's under that event (graphstore.CommentFacts, graphstore.AuthorFacts), and the comments
// row naming that node and holding no tuples_stored, replying to the comment replyTo names and
// answering the block repliesToBlock names where either is not empty. It answers the comment.
func writeCommentWithItsNodeAsTheStoresRelease(t *testing.T, conn *pgx.Conn, body, replyTo, repliesToBlock string) string {
	t.Helper()
	session := model.Actor{Kind: "session", ID: commentedSession}
	var id string
	inPreviousRelease(t, conn, "a session's comment with its node", func(ctx context.Context, tx pgx.Tx) {
		var at time.Time
		requireExec(t, tx.QueryRow(ctx, `select gen_random_uuid()::text, date_trunc('microseconds', clock_timestamp())`).Scan(&id, &at),
			"draw the comment's id and moment")
		event := recordInPreviousRelease(t, tx, "comment.created", session, map[string]any{"id": id})
		node, tuples := graphstore.CommentFacts(nil, id, session, at, event, "", "", "CORE", nil)
		authors, authored, _ := graphstore.AuthorFacts(graphstore.KindComment, id, session, at, event)
		requireExec(t, graphstore.Write(ctx, tx, append([]graphstore.Node{node}, authors...), append(tuples, authored...)),
			"write the comment's node")
		execInPreviousRelease(t, tx, "the comment's row", `
			insert into comments (id, issue_key, author, body, reply_to, replies_to_block, node_kind, node_address, created_at)
			values ($1::uuid, 'CORE-1', $2, $3, nullif($4, '')::uuid, nullif($5, ''), $6, $1, $7)
		`, id, actorBytes(t, session), body, replyTo, repliesToBlock, graphstore.KindComment, at)
	})
	return id
}
