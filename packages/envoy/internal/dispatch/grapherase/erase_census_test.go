package grapherase

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The people and sessions that only the comments of this file name: the session that wrote one,
// the person whose token proved that session hers, the person who wrote another, and the sessions
// the third one's mentions were sent to - one the listener took, one that failed and was sent the
// comment's fallback copy on its own topic, and one that failed and was sent nothing.
const (
	erasedAuthorSession = "ses_erase_author"
	erasedAuthorOwner   = "dana"
	erasedAuthorPerson  = "bob"
	erasedReached       = "ses_erase_reached"
	erasedFallback      = "ses_erase_fallback"
	erasedUnreached     = "ses_erase_unreached"
)

// commentsOnlyTheyName are the three comments seedCommentsOnlyTheyName writes.
type commentsOnlyTheyName struct {
	bySession, byPerson, mentioning string
}

// appendEventBy writes one event of issue by actor with payload in tx, as the log holds it, and
// answers its id.
func appendEventBy(t *testing.T, tx pgx.Tx, issue, kind string, actor model.Actor, payload any) int64 {
	t.Helper()
	encodedActor, err := json.Marshal(actor)
	if err != nil {
		t.Fatalf("encode the actor of a %s event: %v", kind, err)
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload of a %s event: %v", kind, err)
	}
	var id int64
	if err := tx.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, coalesce((select max(seq) from events where issue_key = $1), 0) + 1, $2,
		        $3::jsonb, $4::jsonb, false)
		returning id`, issue, kind, encodedActor, encodedPayload).Scan(&id); err != nil {
		t.Fatalf("append a %s event of %s: %v", kind, issue, err)
	}
	return id
}

// writeIssueComment writes a comment on issue CORE-1 by author as the API's createComment stores
// it: the row, with the author as the row's author, and the comment.created event under that
// same actor, whose payload is the comment with its mentions. id is the comment's id, and empty
// for one the database draws. It answers the comment's id and its event's.
func writeIssueComment(t *testing.T, database *store.Store, id string, author model.Actor, body string, mentions ...model.Mention) (string, int64) {
	t.Helper()
	ctx := context.Background()
	stored, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode the author %+v: %v", author, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comment by %+v: %v", author, err)
	}
	defer tx.Rollback(ctx)
	comment := model.Comment{IssueKey: new("CORE-1"), Author: author, Body: body}
	if err := tx.QueryRow(ctx, `
		insert into comments (id, issue_key, author, body)
		values (coalesce(nullif($1, '')::uuid, gen_random_uuid()), $2, $3, $4)
		returning id::text, created_at
	`, id, comment.IssueKey, stored, comment.Body).Scan(&comment.ID, &comment.CreatedAt); err != nil {
		t.Fatalf("write the comment by %+v: %v", author, err)
	}
	comment.Mentions, comment.Deliveries = append([]model.Mention{}, mentions...), []model.CommentDelivery{}
	event := appendEventBy(t, tx, "CORE-1", "comment.created", author, model.CommentEventPayload{Comment: comment})
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the comment by %+v: %v", author, err)
	}
	return comment.ID, event
}

// settleMention writes comment's mention of session and its first attempt, settled in state as
// the mention sender's settle settles it: the attempt's row, the comment.delivery receipt, and
// the session it reached where the listener took it, under that receipt (graphstore.QueueReached,
// which records nothing for a failed one).
func settleMention(t *testing.T, database *store.Store, comment string, by model.Actor, session, state string) {
	t.Helper()
	ctx := context.Background()
	target := "session:" + session
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settle of %s: %v", target, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into comment_mentions (comment_id, target, delivery, resolved_session_id) values ($1, $2, 'steer', $3)
	`, comment, target, session); err != nil {
		t.Fatalf("write the mention of %s: %v", target, err)
	}
	var attemptedAt time.Time
	if err := tx.QueryRow(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
		values ($1, $2, 1, 'steer', $3, $4)
		returning created_at
	`, comment, target, session, state).Scan(&attemptedAt); err != nil {
		t.Fatalf("write the attempt at %s: %v", target, err)
	}
	receipt := appendEventBy(t, tx, "CORE-1", "comment.delivery", by, model.CommentDeliveryEventPayload{
		CommentID: comment, Target: target, Attempt: 1, Delivery: "steer", SessionID: new(session), State: state,
	})
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		return graphstore.QueueReached(b, state, session, by, attemptedAt, receipt)
	}); err != nil {
		t.Fatalf("record the session the attempt at %s reached: %v", target, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the settle of %s: %v", target, err)
	}
}

// seedCommentsOnlyTheyName is seedAgreeingStore with three comments on issue CORE-1, each by an
// author no other row of the database names, and each written as the API writes it with its
// comment.created event (writeIssueComment): one by a session a person's token proved hers, one by
// a person, and one by alice that mentions three sessions nothing else names. The first mention's
// attempt was taken by the listener, the second's failed and the outbox published the comment to
// that session's own topic instead, and the third's failed with nothing published. The backfill
// then gives the three comments their nodes and their authors', and the session the fallback copy
// reached its node, as it gave the seed's own comment. The store census reads zero over it.
func seedCommentsOnlyTheyName(t *testing.T, database *store.Store) commentsOnlyTheyName {
	t.Helper()
	ctx := context.Background()
	seedAgreeingStore(t, database)
	owner := erasedAuthorOwner
	proved := model.Actor{Kind: "session", ID: erasedAuthorSession, Owner: &owner, Act: &model.ActorAct{Session: erasedAuthorSession}}
	alice := model.Actor{Kind: "user", ID: "alice"}
	var seeded commentsOnlyTheyName
	seeded.bySession, _ = writeIssueComment(t, database, "", proved, "A remark a proved session wrote.")
	seeded.byPerson, _ = writeIssueComment(t, database, "", model.Actor{Kind: "user", ID: erasedAuthorPerson}, "A remark a person wrote.")
	var created int64
	seeded.mentioning, created = writeIssueComment(t, database, "", alice, "A remark three sessions were sent.")
	settleMention(t, database, seeded.mentioning, alice, erasedReached, model.DeliveryStateSent)
	settleMention(t, database, seeded.mentioning, alice, erasedFallback, model.DeliveryStateFailed)
	settleMention(t, database, seeded.mentioning, alice, erasedUnreached, model.DeliveryStateFailed)
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_fallback_copies (comment_id, destination, event_id, session_id, settled_at, published)
		values ($1, $2, $3, $4, now(), true)
	`, seeded.mentioning, contracts.AgentTopicPrefix+erasedFallback, created, erasedFallback); err != nil {
		t.Fatalf("write the fallback copy published to %s: %v", erasedFallback, err)
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comments: %v", err)
	}
	requireCensusZero(t, database, "before the erases")
	requireLiveNodes(t, database, "before the erases")
	return seeded
}

// requireLiveNodes fails unless the store holds a live node of every person and session the
// seeded comments name that a writer records, and none of the session whose attempt failed and
// was sent nothing, which no writer records.
func requireLiveNodes(t *testing.T, database *store.Store, when string) {
	t.Helper()
	for _, node := range [][2]string{
		{graphstore.KindSession, erasedAuthorSession},
		{graphstore.KindPerson, erasedAuthorOwner},
		{graphstore.KindPerson, erasedAuthorPerson},
		{graphstore.KindSession, erasedReached},
		{graphstore.KindSession, erasedFallback},
	} {
		held, found, err := graphstore.ReadNode(context.Background(), database.Pool, node[0], node[1])
		if err != nil || !found || held.State != graphstore.StateLive {
			t.Errorf("%s the store holds %s:%s found %t in state %q (%v), want it live", when, node[0], node[1], found, held.State, err)
		}
	}
	if _, found, err := graphstore.ReadNode(context.Background(), database.Pool, graphstore.KindSession, erasedUnreached); err != nil || found {
		t.Errorf("%s the store holds %s:%s found %t (%v), want no node: nothing reached it", when, graphstore.KindSession, erasedUnreached, found, err)
	}
}

// R5, R19: the census reads zero after every erase that runs. The erase takes a comment's row and
// the rows that cannot exist without it - its mentions and its delivery attempts among them - and
// keeps its events; it never takes a session, and takes a person only when asked to. So the
// session that wrote an erased comment, the person whose token proved that session hers, the
// person who wrote another and the session a third one's mention reached are each a live node
// after the erases, and the census holds each to a record that is still there: the comment.created
// event that stored its comment, under the actor who wrote it, and the comment.delivery receipt of
// the attempt the listener took. The session the fallback copy reached is held to that copy's row,
// which the erase leaves, and the session whose attempt failed with nothing sent is held to no node
// at all.
func TestTheCensusHoldsWhoAnErasedCommentNamedToItsEvents(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedCommentsOnlyTheyName(t, database)
	requireErased(t, database, commentNodeKind, seeded.bySession)
	requireErased(t, database, commentNodeKind, seeded.byPerson)
	requireErased(t, database, commentNodeKind, seeded.mentioning)
	requireLiveNodes(t, database, "after the erases")
	requireCensusZero(t, database, "after the erases")
}

// R5: a person nothing of the store names any longer may be erased, and the census reads zero
// after that erase as it does after every other. Bob's one comment is erased, which leaves his
// node held by the comment.created event that stored it; then he is erased. The event still names
// him as its author, since the erase keeps every event's actor, and the census holds the store to
// no node for an event that the erase of its person came after.
func TestTheEraseOfAPersonAfterHisCommentLeavesTheCensusAtZero(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedCommentsOnlyTheyName(t, database)
	requireErased(t, database, commentNodeKind, seeded.byPerson)
	requireCensusZero(t, database, "after the comment's erase")
	requireErased(t, database, graphstore.KindPerson, erasedAuthorPerson)
	requireCensusZero(t, database, "after the person's erase")
}

// R5, R19: a person is named by the row of a comment a session the server proved hers wrote. The
// comment's author is the session, whose act names that same session and whose `owner` is dana,
// and the census names dana from that row (graphcensus's commentWriterPeople). Nothing the row
// projects states a tuple into her: the comment's authored_by names the session, and the session's
// alias_of is her one tuple in, from a node no row projects. An erase that went on would leave the
// census naming her from the row with her node gone. So the erase of dana refuses, naming the
// comment, and changes nothing, her node stays live and the census reads zero; once the comment is
// erased she is, and the census reads zero again.
func TestTheEraseRefusesAPersonWhoseProvedSessionsCommentStands(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedCommentsOnlyTheyName(t, database)
	projected := make([]string, len(graphstore.ProjectedTables))
	for index, held := range graphstore.ProjectedTables {
		projected[index] = `exists (select 1 from ` + held.Table + ` p where p.node_kind = t.from_kind and p.node_address = t.from_id)`
	}
	var aliases, stated int64
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where t.kind = $3 and t.from_kind = $4 and t.from_id = $5),
		       count(*) filter (where `+strings.Join(projected, " or ")+`)
		from graph_tuples t where t.to_kind = $1 and t.to_id = $2`,
		graphstore.KindPerson, erasedAuthorOwner, graphstore.TupleAliasOf, graphstore.KindSession, erasedAuthorSession).Scan(&aliases, &stated); err != nil {
		t.Fatalf("read the tuples into %s:%s: %v", graphstore.KindPerson, erasedAuthorOwner, err)
	}
	if aliases != 1 || stated != 0 {
		t.Fatalf("%s:%s holds %d alias_of tuple(s) from %s:%s and %d tuple(s) from a node a row projects, want one and none, or this test would prove nothing",
			graphstore.KindPerson, erasedAuthorOwner, aliases, graphstore.KindSession, erasedAuthorSession, stated)
	}

	requireRefused(t, database, graphstore.KindPerson, erasedAuthorOwner, commentNodeKind+":"+seeded.bySession)
	if held, found, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindPerson, erasedAuthorOwner); err != nil || !found || held.State != graphstore.StateLive {
		t.Errorf("after the refused erase the store holds %s:%s found %t in state %q (%v), want it live", graphstore.KindPerson, erasedAuthorOwner, found, held.State, err)
	}
	requireCensusZero(t, database, "after the refused erase of the person")

	requireErased(t, database, commentNodeKind, seeded.bySession)
	requireErased(t, database, graphstore.KindPerson, erasedAuthorOwner)
	requireCensusZero(t, database, "after the erases of the comment and the person")
}
