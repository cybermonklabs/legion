package docs

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The sessions of this test: the one a seeded direct conversation holds, the one the previous
// release's issue message is sent to and its attempt reaches, the follower its events add and
// remove, and the one its outbox's fallback copy reaches.
const (
	kindsReadSession     = "ses-kinds-read"
	kindsMessageSession  = "ses-kinds-message"
	kindsFollowerSession = "ses-kinds-follower"
	kindsFallbackSession = "ses-kinds-fallback"
)

// Every kind of the inbox's caches the release before this one writes after the backfill is
// marked by this build's block-index ticks, with no second backfill, and every session it names
// takes its node: R9 says of the unmarked class "That class reaches zero when the backfill ends
// ... No class is permanent", and a row only a second backfill marks would stand for good.
//
// The release before writes those rows with no marker and states nothing in the store. Each tick
// that owes the object pass runs, one batch each, the backfill's own pass of each kind over the
// rows it still owes (graphcensus.CatchUpOwedRows), and the tick owes it while another build
// serves, on the first tick after, and on an event after its cursor that leaves such a row owed
// (graphcensus.EventsOweObjectPass). A decline, which holds no row but its event, and a session
// an event names, are given the backfill's write by the event read itself
// (graphcensus.CatchUpEvents), which reads each event once.
//
// The test seeds a comment, an ask and a direct conversation before the store exists, and runs the
// backfill. A process of that release then takes every period of the lease, so no tick of this
// build runs while it serves, and writes, each with its event: an issue message with a body block,
// its link and its citation, sent to a session that takes its attempt; an ask with the same; a new
// block of the seeded comment, as its edit's index writes one; bob's decline of the seeded ask; a
// follower of that ask added and removed; and alice's read of the conversation, her clear of it,
// her read of its reply, her dismissal of it and her snooze of the ask. It leaves, and this
// build's next tick must mark every one of those rows, state the decline, and give each session
// its node, and every pass must owe nothing. Then a process of that release serves beside this
// build and its outbox publishes a fallback copy to a session, which appends no event; this
// build's tick while it serves must give that session its node. Its control is the catch-up's
// new passes and the event catch-up taken out: each class it names stays owed.
func TestEveryInboxKindThePreviousReleaseWritesAfterTheBackfillIsMarkedByTheTick(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "inbox-kinds"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	seeded := seedInboxKinds(t, database)
	drainBlockIndex(t, service, "seed")
	takeTheItemBlocksOff(t, database)
	takeTheStoreOff(t, database)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	drainBlockIndex(t, service, "the backfill's mark")
	requireCensusReadsZero(t, database, "after this build drained the backfill's mark")
	requireNoInboxRowOwed(t, database, "after the backfill")

	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	previous := previousReleaseConn(t, database)
	classes := writeInboxKindsAsThePreviousRelease(t, previous, seeded)
	requireInboxClassesOwed(t, database, classes, true, "after the previous release wrote them, before this build's tick")
	previousLeaves()
	alone := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), gave %d object(s) their nodes, marked %d inbox row(s), another build %t",
		alone.Events, alone.ObjectNodes, alone.InboxRows, alone.AnotherBuild)
	requireInboxClassesOwed(t, database, classes, false, "after the first tick after the previous release left, with no second backfill")
	requireNoInboxRowOwed(t, database, "after the first tick after the previous release left")
	requireCensusReadsZero(t, database, "after the previous release wrote every kind of the inbox's caches, with no second backfill")

	// A process of that release serves beside this build, and its outbox publishes a fallback copy
	// to a session, which appends no event.
	previousLeaves = registerProcess(t, database, previousReleaseCommit)
	publishFallbackCopyAsThePreviousRelease(t, previous, seeded.comment, kindsFallbackSession)
	fallback := []inboxClass{sessionClass("the session a fallback copy reached", kindsFallbackSession)}
	requireInboxClassesOwed(t, database, fallback, true, "after the previous release's outbox published a fallback copy, before this build's tick")
	roll := awaitedBlockIndexTick(t, service)
	if !roll.AnotherBuild {
		t.Errorf("the tick while the previous release served found no other build")
	}
	requireInboxClassesOwed(t, database, fallback, false, "after a tick while the previous release served")
	requireNoInboxRowOwed(t, database, "after a tick while the previous release served")
	previousLeaves()
	awaitedBlockIndexTick(t, service)
	requireCensusReadsZero(t, database, "after the previous release's outbox published a fallback copy and it left, with no second backfill")
}

// seededInboxKinds is what seedInboxKinds writes before the store exists: alice's comment on CORE-1,
// her ask on it, which waits on bob, and her direct message to kindsReadSession with that session's
// reply.
type seededInboxKinds struct {
	comment, ask, root, reply string
}

// seedInboxKinds writes, as rows written before the store existed, alice's comment on CORE-1 and a
// session's, the issue's route, alice's ask on CORE-1 asked of bob, the one person who may decline
// it, and her direct conversation with kindsReadSession, its root and the session's reply, so the
// census can pass only on agreement.
func seedInboxKinds(t *testing.T, database *store.Store) seededInboxKinds {
	t.Helper()
	ctx := context.Background()
	var seeded seededInboxKinds
	for _, seed := range []struct {
		what, statement string
		args            []any
		id              *string
	}{
		{"alice's comment", `insert into comments (issue_key, author, body)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'A remark written before the store existed.') returning id::text`, nil, &seeded.comment},
		{"a session's comment", `insert into comments (issue_key, author, body)
			values ('CORE-1', '{"kind":"session","id":"previous-release-kinds-session"}', 'A session''s remark written before the store existed.')
			returning id::text`, nil, new(string)},
		{"alice's ask", `insert into asks (issue_key, author, question, asked_of)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'Ready before the store existed?', 'bob') returning id::text`, nil, &seeded.ask},
		{"alice's direct message", `insert into messages (author, body, target)
			values ('{"kind":"user","id":"alice"}', 'Can you look?', $1) returning id::text`, []any{"session:" + kindsReadSession}, &seeded.root},
	} {
		if err := database.Pool.QueryRow(ctx, seed.statement, seed.args...).Scan(seed.id); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}
	session, err := json.Marshal(model.Actor{Kind: "session", ID: kindsReadSession})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (author, body, in_reply_to, target) values ($1, 'Looking.', $2::uuid, $3) returning id::text
	`, session, seeded.root, "session:"+kindsReadSession).Scan(&seeded.reply); err != nil {
		t.Fatalf("seed the session's reply: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set route = 'role:previous-release-kinds-role' where key = 'CORE-1'`); err != nil {
		t.Fatalf("seed the route: %v", err)
	}
	return seeded
}

// takeTheItemBlocksOff clears the node columns of every block and link row of a comment, a message
// and an ask, as the rows of a body indexed before the store existed name none, so takeTheStoreOff
// can then delete the block nodes the seed's ticks gave them.
func takeTheItemBlocksOff(t *testing.T, database *store.Store) {
	t.Helper()
	for _, table := range []string{"comment_blocks", "message_blocks", "ask_blocks", "comment_block_refs", "message_block_refs", "ask_block_refs"} {
		if _, err := database.Pool.Exec(context.Background(), `update `+table+` set node_kind = null, node_address = null`); err != nil {
			t.Fatalf("take the store off the rows of %s: %v", table, err)
		}
	}
}

// inboxClass is one class of what the release before writes that the store does not hold yet: what
// it is, and the read of how many of its rows stand owed, an integer.
type inboxClass struct {
	name, owed string
	args       []any
}

// sessionClass is the class of a session the store holds no node of.
func sessionClass(name, session string) inboxClass {
	return inboxClass{name, `select count(*) from (select 1) one
		where not exists (select 1 from graph_nodes where kind = 'session' and id = $1)`, []any{session}}
}

// requireInboxClassesOwed fails the test unless every class reads some row owed, where owed is set,
// or none, where it is not.
func requireInboxClassesOwed(t *testing.T, database *store.Store, classes []inboxClass, owed bool, when string) {
	t.Helper()
	for _, class := range classes {
		var rows int64
		if err := database.Pool.QueryRow(context.Background(), class.owed, class.args...).Scan(&rows); err != nil {
			t.Fatalf("read how many rows of %s stand owed: %v", class.name, err)
		}
		switch {
		case owed && rows == 0:
			t.Fatalf("%s owes nothing %s, so this test proves nothing of it", class.name, when)
		case !owed && rows != 0:
			t.Errorf("%d row(s) of %s still owed %s, want none", rows, class.name, when)
		}
	}
}

// requireNoInboxRowOwed fails the test unless every pass the block-index tick catches up, and the
// decline pass, owes nothing (graphcensus.InboxRowsOwed).
func requireNoInboxRowOwed(t *testing.T, database *store.Store, when string) {
	t.Helper()
	owed, err := graphcensus.InboxRowsOwed(context.Background(), database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	for kind, rows := range owed {
		if rows != 0 {
			t.Errorf("the %s pass owes %d row(s) %s, want none", kind, rows, when)
		}
	}
}

// writeInboxKindsAsThePreviousRelease has the stand-in for the release before this one write,
// through conn and each with the event that release appends in its transaction, an issue message
// and an ask with their bodies, a new block of the seeded comment, bob's decline of the seeded ask,
// a follower of it added and removed, and alice's read state of the seeded conversation and ask. It
// answers each class of what those writes leave the store owed.
func writeInboxKindsAsThePreviousRelease(t *testing.T, conn *pgx.Conn, seeded seededInboxKinds) []inboxClass {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	var message, ask string
	inPreviousRelease(t, conn, "an issue message with its body and its attempt", func(ctx context.Context, tx pgx.Tx) {
		var at time.Time
		requireExec(t, tx.QueryRow(ctx, `
			insert into messages (issue_key, author, body, target) values ('CORE-1', $1, 'After dispatch://CORE-2.', $2)
			returning id::text, created_at
		`, actorBytes(t, alice), "session:"+kindsMessageSession).Scan(&message, &at), "write the message")
		event := recordInPreviousRelease(t, tx, "message.created", alice, map[string]any{"id": message})
		node := "message/" + message + "#b1"
		execInPreviousRelease(t, tx, "the message's block", `
			insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, created_at, made_at, changed_at)
			values ($1::uuid, 'b1', $2, 0, 'paragraph', '', 'After dispatch://CORE-2.', now(), now(), now())`, message, node)
		execInPreviousRelease(t, tx, "the message's link", `
			insert into message_block_refs (message_id, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
			values ($1::uuid, 'b1', $2, 'issue', 'CORE-2', 'mentions', now(), $3)`, message, node, event)
		execInPreviousRelease(t, tx, "the message's citation", `
			insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
			values ('message', $1, 'issue', 'CORE-2', 'mentions', now(), $2)`, message, event)
		execInPreviousRelease(t, tx, "the message's attempt", `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state) values ($1::uuid, 1, 'aside', $2, 'sent')`,
			message, kindsMessageSession)
		recordInPreviousRelease(t, tx, "message.delivery", model.Actor{Kind: "session", ID: kindsMessageSession}, model.MessageDeliveryEventPayload{
			MessageID: message, Attempt: 1, Delivery: "aside", SessionID: kindsMessageSession, Target: "session:" + kindsMessageSession, State: "sent",
		})
	})
	inPreviousRelease(t, conn, "an ask with its body", func(ctx context.Context, tx pgx.Tx) {
		var at time.Time
		requireExec(t, tx.QueryRow(ctx, `
			insert into asks (issue_key, author, question) values ('CORE-1', $1, 'After dispatch://CORE-2?') returning id::text, created_at
		`, actorBytes(t, alice)).Scan(&ask, &at), "write the ask")
		event := recordInPreviousRelease(t, tx, "ask.opened", alice, model.AskEventPayload{Ask: model.Ask{
			ID: ask, IssueKey: new("CORE-1"), Author: alice, CreatedAt: at,
		}})
		node := "ask/" + ask + "#b1"
		execInPreviousRelease(t, tx, "the ask's block", `
			insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, created_at, made_at, changed_revision, changed_at)
			values ($1::uuid, 'b1', $2, 0, 'paragraph', '', 'After dispatch://CORE-2?', now(), now(), 1, now())`, ask, node)
		execInPreviousRelease(t, tx, "the ask's link", `
			insert into ask_block_refs (ask_id, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
			values ($1::uuid, 'b1', $2, 'issue', 'CORE-2', 'mentions', now(), $3)`, ask, node, event)
		execInPreviousRelease(t, tx, "the ask's citation", `
			insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
			values ('ask', $1, 'issue', 'CORE-2', 'mentions', now(), $2)`, ask, event)
	})
	inPreviousRelease(t, conn, "a new block of the seeded comment", func(ctx context.Context, tx pgx.Tx) {
		execInPreviousRelease(t, tx, "the comment's new block", `
			insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, created_at,
			                            made_at, changed_revision, changed_at)
			values ($1::uuid, 'b9', $2, 9, 'paragraph', '', 'An added paragraph.', now(), now(), 2, now())`,
			seeded.comment, "comment/"+seeded.comment+"#b9")
		recordInPreviousRelease(t, tx, "comment.edited", alice, map[string]any{"id": seeded.comment})
	})
	var declined int64
	inPreviousRelease(t, conn, "bob's decline of the seeded ask", func(ctx context.Context, tx pgx.Tx) {
		declined = recordInPreviousRelease(t, tx, "ask.resolved", model.Actor{Kind: "user", ID: "bob"}, map[string]any{
			"id": seeded.ask, "resolution": map[string]any{"kind": model.AskResolutionDeclined, "at": time.Now().UTC()},
		})
	})
	inPreviousRelease(t, conn, "a follower of the seeded ask, added and removed", func(ctx context.Context, tx pgx.Tx) {
		for _, kind := range []string{"ask.follower_added", "ask.follower_removed"} {
			recordInPreviousRelease(t, tx, kind, alice, model.AskFollowerEventPayload{AskID: seeded.ask, SessionID: kindsFollowerSession, By: alice})
		}
	})
	inPreviousRelease(t, conn, "alice's read state", func(ctx context.Context, tx pgx.Tx) {
		for _, row := range []struct {
			what, statement string
			args            []any
		}{
			{"her read of the conversation", `insert into user_agent_read (login, session_id, read_through) values ('alice', $1, clock_timestamp())`,
				[]any{kindsReadSession}},
			{"her clear of it", `insert into user_agent_state (login, session_id, cleared_before) values ('alice', $1, clock_timestamp())`,
				[]any{kindsReadSession}},
			{"her read of its reply", `insert into user_agent_reply_read (login, session_id, reply_id) values ('alice', $1, $2::uuid)`,
				[]any{kindsReadSession, seeded.reply}},
			{"her dismissal of it", `insert into user_agent_thread_dismissed (login, message_id, dismissed_at) values ('alice', $1::uuid, clock_timestamp())`,
				[]any{seeded.root}},
			{"her snooze of the ask", `insert into user_ask_snooze (login, ask_id, snoozed_until) values ('alice', $1::uuid, clock_timestamp() + interval '1 day')`,
				[]any{seeded.ask}},
		} {
			execInPreviousRelease(t, tx, row.what, row.statement, row.args...)
		}
		recordProjectEventInPreviousRelease(t, tx, "user_agent_state.updated", alice, model.UserAgentStateEventPayload{Login: "alice", SessionID: kindsReadSession})
		recordProjectEventInPreviousRelease(t, tx, "user_state.updated", alice, map[string]any{"login": "alice", "changed": []string{"snoozed"}})
	})

	unmarked := func(table, column, id string) string {
		return `select count(*) from ` + table + ` where ` + column + ` = '` + id + `' and node_kind is null`
	}
	unstated := func(kind, id string) string {
		return `select count(*) from refs where from_kind = '` + kind + `' and from_id = '` + id + `' and not tuples_stored`
	}
	threadRow := func(thread string) string {
		return `select count(*) from user_threads where thread_id = '` + thread + `' and marked_seq is null`
	}
	readState := func(table string) string {
		return `select count(*) from ` + table + ` where login = 'alice' and tuples_stored_at is null`
	}
	return []inboxClass{
		{name: "the message's row", owed: unmarked("messages", "id", message)},
		{name: "the message's block", owed: unmarked("message_blocks", "message_id", message)},
		{name: "the message's link", owed: unmarked("message_block_refs", "message_id", message)},
		{name: "the message's citation", owed: unstated("message", message)},
		{name: "the message's attempt", owed: unmarked("message_deliveries", "message_id", message)},
		sessionClass("the session the message's attempt reached", kindsMessageSession),
		{name: "the message's thread row", owed: threadRow(message)},
		{name: "the ask's tuples", owed: `select count(*) from asks where id = '` + ask + `' and tuples_stored_at is null`},
		{name: "the ask's block", owed: unmarked("ask_blocks", "ask_id", ask)},
		{name: "the ask's link", owed: unmarked("ask_block_refs", "ask_id", ask)},
		{name: "the ask's citation", owed: unstated("ask", ask)},
		{name: "the ask's thread row", owed: threadRow(ask)},
		{name: "the comment's new block", owed: unmarked("comment_blocks", "comment_id", seeded.comment)},
		{name: "bob's decline", owed: `select count(*) from events e where e.id = $1 and not exists (
			select 1 from graph_tuples t where t.to_kind = 'ask' and t.to_id = e.payload->>'id'
			  and t.kind = '` + graphstore.TupleParticipatesIn + `' and t.source_event = e.id)`, args: []any{declined}},
		sessionClass("the follower's session", kindsFollowerSession),
		{name: "alice's read of the conversation", owed: readState("user_agent_read")},
		{name: "alice's clear of it", owed: readState("user_agent_state")},
		{name: "alice's read of its reply", owed: readState("user_agent_reply_read")},
		{name: "alice's dismissal of it", owed: readState("user_agent_thread_dismissed")},
		{name: "alice's snooze of the ask", owed: readState("user_ask_snooze")},
	}
}

// publishFallbackCopyAsThePreviousRelease records, through conn, a fallback copy of comment the
// release before this one's outbox published to session's own topic, as that outbox leaves one: a
// published comment_fallback_copies row, with no event.
func publishFallbackCopyAsThePreviousRelease(t *testing.T, conn *pgx.Conn, comment, session string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), `
		insert into comment_fallback_copies (comment_id, destination, event_id, session_id, published, settled_at)
		values ($1::uuid, $2, (select coalesce(max(id), 0) from events), $3, true, now())
	`, comment, contracts.AgentTopicPrefix+session, session); err != nil {
		t.Fatalf("publish a fallback copy of %s to %s as the previous release: %v", comment, session, err)
	}
}

// inPreviousRelease runs write in a transaction of conn and commits it, as one write of the release
// before this one.
func inPreviousRelease(t *testing.T, conn *pgx.Conn, what string, write func(context.Context, pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	write(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// execInPreviousRelease runs one statement of a write of the release before this one in tx.
func execInPreviousRelease(t *testing.T, tx pgx.Tx, what, statement string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("write %s as the previous release: %v", what, err)
	}
}

// requireExec fails the test on err, a statement's failure to do what.
func requireExec(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s as the previous release: %v", what, err)
	}
}

// actorBytes is actor as an author column or an event's actor holds it.
func actorBytes(t *testing.T, actor model.Actor) []byte {
	t.Helper()
	encoded, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// recordInPreviousRelease appends in tx the event of kind by actor with payload on issue CORE-1, as
// the release before this one appends one, and answers its id.
func recordInPreviousRelease(t *testing.T, tx pgx.Tx, kind string, actor model.Actor, payload any) int64 {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event, err := recordIssueEvent(context.Background(), tx, "CORE-1", kind, actorBytes(t, actor), encoded)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// recordProjectEventInPreviousRelease appends in tx the event of kind by actor with payload owned by
// project CORE, as the release before this one appends a person's read-state event.
func recordProjectEventInPreviousRelease(t *testing.T, tx pgx.Tx, kind string, actor model.Actor, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		with bumped as (update projects set last_seq = last_seq + 1 where key = 'CORE' returning last_seq)
		insert into events (project_key, seq, type, actor, payload, notify)
		select 'CORE', bumped.last_seq, $1, $2, $3, false
		from bumped
	`, kind, actorBytes(t, actor), encoded); err != nil {
		t.Fatalf("append the %s as the previous release: %v", kind, err)
	}
}
