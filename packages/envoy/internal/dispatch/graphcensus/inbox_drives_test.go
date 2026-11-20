package graphcensus

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The drives of the inbox's writers that no site of a package importing this one runs: the
// backfill's passes over the rows an older build wrote, a transition of a delivery attempt that
// records no event, the outbox's settle of a mention attempt nobody will send, and the refs
// grammar's re-index of an older build's text, owed and rebuilt.
const (
	inboxBackfillDrive    = "graphcensus/backfill_inbox.go#backfillInbox, over the rows an older build wrote"
	attemptedDrive        = "graphstore/delivery_attempt.go#Attempted"
	settleAbandonedDrive  = "outbox/mentions.go#settleAbandoned"
	reindexOwedDrive      = "refs/rebuild.go#queue"
	rebuildDrive          = "refs/rebuild.go#replaceInTx"
	abandonedMentionDelay = model.MentionSweepWindow + time.Minute
)

// inboxDrives are those drives. The backfill's runs over a database of its own holding what an
// older build wrote on issue CENS-1 (the addressing writes): an issue message and bob's reply, a
// comment and bob's reply, the comment's body block with its link and its citation, an attempt
// of the comment's mention the listener took, an ask alice opened, and her read of a session's
// conversation; every pass of the backfill before the inbox's has run (Backfill's order), so what
// it reads off graph_tuples is the inbox's passes' own. The transition then moves an attempt this
// build opened on that same database to sent, as a sender's claim transaction moves one. The
// settle runs over a database of its own: the outbox publishing a comment whose mention's attempt
// was opened past the sweep's window and never claimed. The re-index drives run over that same
// database after it.
func inboxDrives(t *testing.T) []StoreDrive {
	t.Helper()
	ctx := context.Background()
	writes := newAddressingWrites(t)
	database := writes.database
	var comment olderWrite
	var owedComment string
	opened := graphstore.CommentAttempt("", "session:ses_claimed", 1)
	abandoned := storetest.Open(t)
	return []StoreDrive{{
		Name: inboxBackfillDrive, Database: database,
		Sites: []string{
			"graphcensus/backfill_inbox.go#writeMessages", "graphcensus/backfill_inbox.go#writeCommentPointers",
			"graphcensus/backfill_inbox.go#writeBlocks", "graphcensus/backfill_inbox.go#writeLinks",
			"graphcensus/backfill_inbox.go#writeRelations",
			"graphcensus/backfill_inbox.go#writeAttempts", "graphcensus/backfill_inbox_threads.go#writeUserThreads",
			"graphcensus/backfill_inbox_threads.go#writeMarks", "inboxwrite/older.go#RecordOlder",
			"graphstore/to_person.go#ToPerson", "graphstore/from_person.go#FromPerson", "graphstore/write.go#Write",
		},
		Ready: func(t *testing.T) {
			t.Helper()
			writes.createIssue(t, "CENS-1", 1, "U", alice)
			root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
				insert into messages (issue_key, author, body) values ('CENS-1', $1, 'Ship it on Friday?')
				returning id::text, created_at`, actorJSON(t, alice))
			writeAsAnOlderBuild(t, writes, "message.answered", bob, `
				insert into messages (issue_key, author, body, in_reply_to) values ('CENS-1', $1, 'Friday works.', $2)
				returning id::text, created_at`, actorJSON(t, bob), root.id)
			comment = writeAsAnOlderBuild(t, writes, "comment.created", alice, `
				insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
				returning id::text, created_at`, actorJSON(t, alice))
			writeAsAnOlderBuild(t, writes, "comment.created", bob, `
				insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'It is.', $2)
				returning id::text, created_at`, actorJSON(t, bob), comment.id)
			writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
				insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Which day?')
				returning id::text, created_at`, actorJSON(t, alice))
			block := "comment/" + comment.id + "#b1"
			for _, seed := range []struct {
				what, statement string
				args            []any
			}{
				{"the comment's block", `insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
					created_at, made_at, changed_revision, changed_at)
					values ($1::uuid, 'b1', $2, 0, 'paragraph', '', 'Is the copy final?', now(), now(), 1, now())`, []any{comment.id, block}},
				{"the block's link", `insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
					values ($1::uuid, 'b1', $2, 'issue', 'CENS-1', 'mentions', now(), 100)`, []any{comment.id, block}},
				{"the comment's citation", `insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
					values ('comment', $1, 'issue', 'CENS-1', 'mentions', now(), 101)`, []any{comment.id}},
				{"the mention's attempt the listener took", `insert into comment_deliveries (comment_id, target, attempt, delivery, state, session_id)
					values ($1::uuid, 'session:ses_taken', 1, 'aside', 'sent', 'ses_taken')`, []any{comment.id}},
				{"alice's read of a session's conversation", `insert into user_agent_read (login, session_id, read_through)
					values ('alice', 'ses_taken', now())`, nil},
			} {
				if _, err := database.Pool.Exec(ctx, seed.statement, seed.args...); err != nil {
					t.Fatalf("write %s as an older build writes it: %v", seed.what, err)
				}
			}
			for _, pass := range []func(context.Context, *store.Store) (KindBackfill, error){
				backfillBlocks, backfillBlockRefs, backfillComments, backfillRoles, backfillSessions,
			} {
				if done, err := pass(ctx, database); err != nil {
					t.Fatalf("backfill the %s kind before the inbox's passes: %v", done.Kind, err)
				}
			}
			for _, passes := range []func(context.Context, *store.Store) ([]KindBackfill, error){backfillObjectNodes, backfillScopes} {
				if _, err := passes(ctx, database); err != nil {
					t.Fatalf("run the object and scope passes before the inbox's: %v", err)
				}
			}
			for _, pass := range askPasses {
				if done, err := pass(ctx, database); err != nil {
					t.Fatalf("backfill the %s kind before the inbox's passes: %v", done.Kind, err)
				}
			}
		},
		Drive: func(t *testing.T) {
			done, err := backfillInbox(ctx, database)
			if err != nil {
				t.Fatalf("run the inbox's passes: %v", err)
			}
			for _, kind := range done {
				if !kind.Complete {
					t.Fatalf("the inbox's %s pass left %d rows owed: %#v", kind.Kind, kind.Remaining, kind)
				}
			}
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindMessage, graphstore.TupleAuthoredBy}:             {"message_relation"},
			{graphstore.KindMessage, graphstore.TuplePartOf}:                 {"message_relation", "reply"},
			{graphstore.KindMessage, graphstore.TupleRepliesTo}:              {"reply"},
			{graphstore.KindMessage, graphstore.TupleDeliveredTo}:            {"person_delivery"},
			{graphstore.KindComment, graphstore.TupleRepliesTo}:              {"reply"},
			{graphstore.KindComment, graphstore.TuplePartOf}:                 {"reply"},
			{graphstore.KindComment, graphstore.TupleDeliveredTo}:            {"person_delivery"},
			{graphstore.KindComment, graphstore.TupleMentions}:               {"citation"},
			{graphstore.KindPerson, graphstore.TupleParticipatesIn}:          {"participation"},
			{graphstore.KindPerson, graphstore.TupleReadThrough}:             {"read_mark"},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:             {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:                  {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:                {"body_block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:                     {"body_block"},
			{refs.BlockNodeKind, graphstore.TupleMentions}:                   {"body_block_ref"},
			{graphstore.KindDeliveryAttempt, graphstore.AttributeVisibility}: {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttributeState}:      {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptNumber}:       {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptState}:        {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptDuplicate}:    {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.TupleDeliveredTo}:    {"delivery"},
		},
	}, {
		Name: attemptedDrive, Database: database, Sites: []string{attemptedDrive},
		Ready: func(t *testing.T) {
			t.Helper()
			opened.Comment = comment.id
			var at time.Time
			if err := database.Pool.QueryRow(ctx, `select now()`).Scan(&at); err != nil {
				t.Fatalf("read the time the attempt is opened at: %v", err)
			}
			kind, address, write := graphstore.AttemptOpened(opened, graphstore.AttemptWriter, at)
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
					if err := graphstore.QueueNodes(b, write); err != nil {
						return err
					}
					b.Queue("open the attempt", `
						insert into comment_deliveries (comment_id, target, attempt, delivery, state, created_at, node_kind, node_address)
						values ($1::uuid, $2, $3, 'aside', 'pending', $4, $5, $6)
					`, opened.Comment, opened.Target, opened.Attempt, at, kind, address)
					if err := graphstore.QueueCaused(b, write, graphstore.NoEvent); err != nil {
						return err
					}
					return graphstore.QueueAttempted(b, graphstore.AttemptWriter, graphstore.NoEvent, opened)
				})
			}); err != nil {
				t.Fatalf("open the attempt to ses_claimed as a sender opens one: %v", err)
			}
		},
		// A sender's claim, which takes the send and records the session it reached in one
		// transaction that appends no event: the attempt's state and claim, the retraction of the
		// owed_to its pending state stated, and the delivered_to its session.
		Drive: func(t *testing.T) {
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `
					update comment_deliveries set state = 'sent', session_id = 'ses_claimed', claimed_at = now(), envelope_id = 'envelope-1'
					where comment_id = $1::uuid and target = $2 and attempt = $3
				`, opened.Comment, opened.Target, opened.Attempt); err != nil {
					return err
				}
				return graphstore.Attempted(ctx, tx, opened)
			}); err != nil {
				t.Fatalf("move the attempt to sent: %v", err)
			}
		},
		Counts: map[StoredKind][]string{
			{graphstore.KindDeliveryAttempt, graphstore.AttemptState}:      {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptClaimedAt}:  {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptEnvelopeID}: {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.TupleOwedTo}:       {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.TupleDeliveredTo}:  {"delivery"},
		},
	}, {
		Name: settleAbandonedDrive, Database: abandoned,
		Sites: []string{settleAbandonedDrive, "outbox/publisher.go#recordBatch"},
		Ready: func(t *testing.T) { seedAnAbandonedMention(t, abandoned) },
		// The settle of the abandoned attempt under its receipt: its state failed and its error,
		// and the retraction of its owed_to. The outbox then publishes the comment's copy to the
		// session the attempt was for, a delivery the bus made.
		Drive: func(t *testing.T) { publishTheLog(t, abandoned) },
		Counts: map[StoredKind][]string{
			{graphstore.KindDeliveryAttempt, graphstore.AttemptState}: {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.AttemptError}: {"delivery_attempt"},
			{graphstore.KindDeliveryAttempt, graphstore.TupleOwedTo}:  {"delivery_attempt"},
			{graphstore.KindComment, graphstore.TupleDeliveredTo}:     {"bus_delivery"},
		},
	}, {
		// The refs-grammar drain's re-index of a comment an older build wrote, citing ABAN-1:
		// its references and its blocks, as this build's write of it states them.
		Name: reindexOwedDrive, Database: abandoned, Sites: []string{reindexOwedDrive},
		Ready: func(t *testing.T) { owedComment = writeAnOlderCitingComment(t, abandoned) },
		Drive: func(t *testing.T) {
			owed, err := refs.ListOwed(ctx, abandoned.Pool, refs.RefsGrammarOwed, 10)
			if err != nil {
				t.Fatalf("list what the refs grammar owes: %v", err)
			}
			for _, source := range owed {
				if source.ID != owedComment {
					continue
				}
				if indexed, err := refs.ReindexOwed(ctx, abandoned.Pool, source, writersSizingURL); err != nil || !indexed {
					t.Fatalf("re-index comment %s (indexed %v): %v", owedComment, indexed, err)
				}
				return
			}
			t.Fatalf("the refs grammar owes %#v, and not comment %s", owed, owedComment)
		},
		// The comment's citation of ABAN-1 and its body block, with that block's link to the issue.
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleMentions}:   {"citation"},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:      {"body_block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:    {"body_block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"body_block"},
			{refs.BlockNodeKind, graphstore.TupleMentions}:       {"body_block_ref"},
		},
	}, {
		// rebuild-refs over the database, whose one source with references to restate is a
		// second comment an older build wrote, citing ABAN-1.
		Name: rebuildDrive, Database: abandoned, Sites: []string{rebuildDrive},
		Ready: func(t *testing.T) { writeAnOlderCitingComment(t, abandoned) },
		Drive: func(t *testing.T) {
			if _, err := refs.RebuildAll(ctx, abandoned.Pool, writersSizingURL); err != nil {
				t.Fatalf("rebuild every source's references: %v", err)
			}
		},
		// The second comment's citation of ABAN-1, which rebuild-refs states again.
		Counts: map[StoredKind][]string{{graphstore.KindComment, graphstore.TupleMentions}: {"citation"}},
	}}
}

// writersSizingURL is the server URL the re-index drives resolve a citation's link against.
const writersSizingURL = "https://dispatch.sizing.invalid"

// writeAnOlderCitingComment writes a comment of alice's on ABAN-1 citing that issue, as an older
// build writes one: its row alone, which owes the refs grammar its re-index, and its node, as the
// backfill's comment pass writes it. It answers the comment's id.
func writeAnOlderCitingComment(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ('ABAN-1', '{"kind":"user","id":"alice"}', 'Blocked on ABAN-1.')
		returning id::text
	`).Scan(&comment); err != nil {
		t.Fatalf("write the comment citing ABAN-1: %v", err)
	}
	if _, err := backfillComments(ctx, database); err != nil {
		t.Fatalf("write the comment's node: %v", err)
	}
	return comment
}

// seedAnAbandonedMention writes, on issue ABAN-1, which routes to the session ses_abandoned, a
// comment of alice's that mentions that session, its comment.created, and the mention's one
// attempt as this build opens it - its node first, then its row naming it - created past the
// delivery sweep's window and claimed by no sender: an attempt nothing will send. The route's copy
// of the comment is the mention's to decide, so the outbox settles that attempt failed as it
// publishes the comment and publishes the copy (outbox's mentionComment.settleAbandoned). The
// session's node and the comment's own facts are written here too, so the drive reads what the
// settle and the publish append alone.
func seedAnAbandonedMention(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	var comment string
	var created time.Time
	if err := database.Pool.QueryRow(ctx, `
		with project as (
			insert into projects (key, name, default_assignee) values ('ABAN', 'Abandoned', 'alice') returning key
		), issue as (
			insert into issues (key, project_key, number, title, created_by, rank, route)
			select 'ABAN-1', key, 1, 'Mentioned', '{"kind":"user","id":"alice"}', 'U', 'session:ses_abandoned' from project returning key
		)
		insert into comments (issue_key, author, body, created_at)
		select key, '{"kind":"user","id":"alice"}', 'Can you take this?', now() - $1::float8 * interval '1 second' from issue
		returning id::text, created_at
	`, abandonedMentionDelay.Seconds()).Scan(&comment, &created); err != nil {
		t.Fatalf("write the comment that mentions ses_abandoned: %v", err)
	}
	abandonedSession := "ses_abandoned"
	payload, err := json.Marshal(model.CommentEventPayload{Comment: model.Comment{
		ID: comment, IssueKey: new("ABAN-1"), Author: alice, Body: "Can you take this?", CreatedAt: created,
		Mentions: []model.Mention{{Target: "session:" + abandonedSession, Delivery: "aside", SessionID: &abandonedSession}},
	}})
	if err != nil {
		t.Fatalf("encode the comment's comment.created: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_mentions (comment_id, target, delivery) values ($1::uuid, 'session:ses_abandoned', 'aside');
	`, comment); err != nil {
		t.Fatalf("write the comment's mention: %v", err)
	}
	// The event moves the issue's last_seq with it, as an append does, so the receipt the settle
	// appends next takes the seq after it.
	if _, err := database.Pool.Exec(ctx, `
		with appended as (
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ('ABAN-1', 1, 'comment.created', '{"kind":"user","id":"alice"}', $1::jsonb, true)
			returning seq
		)
		update issues set last_seq = (select seq from appended) where key = 'ABAN-1'
	`, payload); err != nil {
		t.Fatalf("append the comment's comment.created: %v", err)
	}
	key := graphstore.CommentAttempt(comment, "session:ses_abandoned", 1)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{
			graphstore.AttemptNode(key, alice, created, graphstore.NoEvent),
			graphstore.SessionNode("ses_abandoned", alice, created, graphstore.NoEvent),
		}, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, state, created_at, node_kind, node_address)
			values ($1::uuid, $2, 1, 'aside', 'pending', $3, $4, $5)
		`, comment, key.Target, created, graphstore.KindDeliveryAttempt, key.ID()); err != nil {
			return err
		}
		return graphstore.Attempted(ctx, tx, key)
	}); err != nil {
		t.Fatalf("open the mention's attempt and leave it unclaimed: %v", err)
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill what the comment's write states, before the outbox publishes it: %v", err)
	}
}
