package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// R5: a mention "runs from the comment, issue message or document block that holds it to the
// identity it points at", and it follows that text as the item's other edges follow it: a write
// that takes an identity out of the text retracts the mention (refs' unnamedMentions), and what the
// mention caused stands - the person's delivered_to and participates_in (R2, R4).
//
// In each case alice writes a comment naming a person and then edits it to name the reviewer role
// beside the person, so the role is a mention of the text alone and no delivery route of the
// comment (comment_mentions): the References read into the role lists the comment (refs.Edges),
// the count beside it counts it (refs.BacklinkCounts), and the person's one read carries the
// comment with the mentions kind. Then a write takes both out of the text - an edit to text that
// names neither, and the author's delete, which clears the body - and the newest mentions tuple of
// each pair is a retraction under that write's event, the read into the role no longer lists the
// comment and counts one fewer, and the person's entry of the comment has lost the mentions kind
// and still carries the comment's delivery to them and their participation in its thread.
func TestAnEditAndADeleteRetractTheIdentityMentionsTheirTextNoLongerNames(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "RETR", "Retracted mentions", "before")
	ctx := context.Background()
	reader := model.Actor{Kind: "user", ID: "alice"}
	const role = "reviewer"
	intoTheRole := func(what string) ([]string, int) {
		t.Helper()
		edges, err := refs.Edges(ctx, database.Pool, reader, refs.Query{Direction: "in", Kind: graphstore.KindRole, ID: role})
		if err != nil {
			t.Fatalf("%s: read the edges into role %s: %v", what, role, err)
		}
		listed := make([]string, len(edges))
		for index, edge := range edges {
			listed[index] = edge.Kind + " from " + edge.Node.Kind + " " + edge.Node.ID
		}
		counts, err := refs.BacklinkCounts(ctx, database.Pool, reader, graphstore.KindRole, []string{role}, nil)
		if err != nil {
			t.Fatalf("%s: count the edges into role %s: %v", what, role, err)
		}
		return listed, counts[role]
	}
	// entryOf reads the person's one read until its entry of the comment holds, through the
	// horizon the read answers at (awaitHorizonRead), and answers that entry's relations.
	entryOf := func(what, commentID string, holds func(kinds []string) bool) []string {
		t.Helper()
		answer := awaitHorizonRead(t, database, what, func() meInboxResponse {
			return readMeInboxAs(t, handler, mentionedPerson, nil)
		}, func(answer meInboxResponse) bool {
			entry, found := meInboxEntryOfNode(answer, graphstore.KindComment, commentID)
			return found && holds(meInboxKinds(entry))
		}, func(answer meInboxResponse) string {
			entry, _ := meInboxEntryOfNode(answer, graphstore.KindComment, commentID)
			return fmt.Sprintf("entries %v, the comment's relations %v", meInboxNodes(answer), meInboxKinds(entry))
		})
		entry, _ := meInboxEntryOfNode(answer, graphstore.KindComment, commentID)
		return meInboxKinds(entry)
	}
	const (
		mentioned   = graphstore.TupleMentions + "/" + meInboxInbound
		delivered   = graphstore.TupleDeliveredTo + "/" + meInboxInbound
		takesPartIn = graphstore.TupleParticipatesIn + "/" + meInboxOutbound
	)
	for _, retracting := range []struct {
		name, event string
		write       func(t *testing.T, commentID string)
	}{{
		name: "an edit to text that names neither", event: "comment.edited",
		write: func(t *testing.T, commentID string) {
			requireEdit(t, "edit the comment to name neither", editCommentAs(t, handler, commentID, "Over to the team for the copy.", nil, nil))
		},
	}, {
		name: "the author's delete", event: "comment.deleted",
		write: func(t *testing.T, commentID string) { deleteThreadComment(t, handler, commentID, "alice") },
	}} {
		t.Run(retracting.name, func(t *testing.T) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Over to dispatch://person/" + mentionedPerson + " for the copy.",
			}, "alice")
			requireEdit(t, "edit the comment to name the role too", editCommentAs(t, handler, comment.ID,
				"Over to dispatch://person/"+mentionedPerson+" and dispatch://role/"+role+" for the copy.", nil, nil))
			named := writeEventOf(t, database, "comment.edited", comment.ID)
			requireNewestMention(t, database, comment.ID, graphstore.KindRole, role, true, named)
			listedBefore, countedBefore := intoTheRole("before the retraction")
			from := graphstore.TupleMentions + " from " + graphstore.KindComment + " " + comment.ID
			if !slices.Contains(listedBefore, from) || countedBefore != len(listedBefore) {
				t.Fatalf("before the retraction the read into role %s lists %v and counts %d, want the comment's mention among them, counted",
					role, listedBefore, countedBefore)
			}
			entryOf("the person's read before the retraction", comment.ID, func(kinds []string) bool {
				return slices.Contains(kinds, mentioned) && slices.Contains(kinds, delivered) && slices.Contains(kinds, takesPartIn)
			})

			retracting.write(t, comment.ID)
			retracted := writeEventOf(t, database, retracting.event, comment.ID)
			requireNewestMention(t, database, comment.ID, graphstore.KindRole, role, false, retracted)
			requireNewestMention(t, database, comment.ID, graphstore.KindPerson, mentionedPerson, false, retracted)
			listedAfter, countedAfter := intoTheRole("after the retraction")
			if slices.Contains(listedAfter, from) || countedAfter != countedBefore-1 || countedAfter != len(listedAfter) {
				t.Errorf("after the retraction the read into role %s lists %v and counts %d, want the comment's mention gone and %d counted",
					role, listedAfter, countedAfter, countedBefore-1)
			}
			kinds := entryOf("the person's read after the retraction", comment.ID, func(kinds []string) bool {
				return !slices.Contains(kinds, mentioned)
			})
			if !slices.Contains(kinds, delivered) || !slices.Contains(kinds, takesPartIn) {
				t.Errorf("after the retraction the person's entry of the comment carries %v, want its delivery %s and the participation %s still",
					kinds, delivered, takesPartIn)
			}
		})
	}
}

// requireNewestMention fails unless the newest mentions tuple out of comment commentID to
// toKind/toID asserts the mention as asserted says, under event.
func requireNewestMention(t *testing.T, database *store.Store, commentID, toKind, toID string, asserted bool, event int64) {
	t.Helper()
	var stands bool
	var under int64
	err := database.Pool.QueryRow(context.Background(), `
		select asserted, source_event from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, graphstore.KindComment, commentID, graphstore.TupleMentions, toKind, toID).Scan(&stands, &under)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the store holds no mention out of comment %s to %s %s", commentID, toKind, toID)
	}
	if err != nil {
		t.Fatalf("read the newest mention out of comment %s to %s %s: %v", commentID, toKind, toID, err)
	}
	if stands != asserted || under != event {
		t.Errorf("the newest mention out of comment %s to %s %s asserts %t under event %d, want %t under %d",
			commentID, toKind, toID, stands, under, asserted, event)
	}
}

// R5: a mention follows the text that holds it from the first index of that text on. The backfill
// states the identity mentions an older build's comment names without indexing the comment's blocks
// (graphcensus' commentPointersPass), so the comment holds current mentions while its block state
// records no revision, which refs.ReadItemIndex reads as an item never indexed; and the comment's
// delivery routes (its comment_mentions rows) are mentions out of it too, which no change of its
// text retracts (refs' unnamedMentions).
//
// In each case alice's comment is written as the release before writes one - its row, naming a
// person and the reviewer role, a delivery route to an agent's session, and its comment.created -
// and nothing indexes its blocks: the backfill states the three mentions, the person's delivery and
// the person's part in the comment's thread, and the comment's block state stays at revision 0.
// Then this build's first write of the comment's text takes both names out - an edit to text that
// names neither, and the author's delete - and indexes it at revision 1: the newest mention of the
// person and of the role is a retraction under that write's event, the session's route stands, and
// the person keeps the comment's delivery and their part in its thread. Its control is
// refs.ReadItemIndex reading the mentions of an item no block-aware build indexed as none: the
// write retracts neither name; and with those mentions read but not the routes, it retracts the
// session's route too.
func TestTheFirstIndexOfAnOlderBuildsCommentRetractsWhatItsTextNoLongerNames(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "FRST", "A first index", "before")
	ctx := context.Background()
	const role, routed = "reviewer", "s-routed"
	revisionOf := func(t *testing.T, commentID string) int {
		t.Helper()
		var revision int
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select revision from comment_block_state where comment_id = $1::uuid), 0)
		`, commentID).Scan(&revision); err != nil {
			t.Fatalf("read the block state of comment %s: %v", commentID, err)
		}
		return revision
	}
	for _, correcting := range []struct {
		name, event string
		write       func(t *testing.T, commentID string)
	}{{
		name: "an edit to text that names neither", event: "comment.edited",
		write: func(t *testing.T, commentID string) {
			requireEdit(t, "edit the comment to name neither", editCommentAs(t, handler, commentID, "Over to the team for the copy.", nil, nil))
		},
	}, {
		name: "the author's delete", event: "comment.deleted",
		write: func(t *testing.T, commentID string) { deleteThreadComment(t, handler, commentID, "alice") },
	}} {
		t.Run(correcting.name, func(t *testing.T) {
			comment, created := commentAsTheReleaseBefore(t, database, issue.Key,
				"Over to dispatch://person/"+mentionedPerson+" and dispatch://role/"+role+" for the copy.", "session:"+routed)
			if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
				t.Fatalf("backfill the older build's comment: %v", err)
			}
			if revision := revisionOf(t, comment); revision != 0 {
				t.Fatalf("after the backfill the comment's block state records revision %d, want 0, never indexed: this test proves nothing", revision)
			}
			requireNewestMention(t, database, comment, graphstore.KindPerson, mentionedPerson, true, created)
			requireNewestMention(t, database, comment, graphstore.KindRole, role, true, created)
			requireRelationStands(t, database, "the route before the write",
				graphstore.KindComment, comment, graphstore.TupleMentions, graphstore.KindSession, routed)

			correcting.write(t, comment)
			corrected := writeEventOf(t, database, correcting.event, comment)
			if revision := revisionOf(t, comment); revision != 1 {
				t.Fatalf("after this build's write the comment's block state records revision %d, want 1, its first index", revision)
			}
			requireNewestMention(t, database, comment, graphstore.KindPerson, mentionedPerson, false, corrected)
			requireNewestMention(t, database, comment, graphstore.KindRole, role, false, corrected)
			requireRelationStands(t, database, "the route after the write",
				graphstore.KindComment, comment, graphstore.TupleMentions, graphstore.KindSession, routed)
			requireRelationStands(t, database, "the person's delivery after the write",
				graphstore.KindComment, comment, graphstore.TupleDeliveredTo, graphstore.KindPerson, mentionedPerson)
			requireRelationStands(t, database, "the person's part in the thread after the write",
				graphstore.KindPerson, mentionedPerson, graphstore.TupleParticipatesIn, graphstore.KindComment, comment)
		})
	}
}

// R5's mentions follow an item's latest text, and R4's participation outlives the mention that
// brought it, whichever order the writes of two builds are caught up in. An older build's edit is
// stated by the catch-up of its event (graphcensus' writeOlderEdit), which can run after this build
// has written the same comment again: this build's write held the comment's mentions to its own
// text as it wrote, and the older edit, caught up late, undoes none of it.
//
// In each case alice writes a comment through this build naming the reviewer role, with a delivery
// route to an agent's session. A build of the release before then edits it to name a person and not
// the role, and the trigger queues the edit's event. Before any catch-up this build writes the
// comment again - an edit back to text that names the role and not the person, and the author's
// delete - and only then does the backfill catch the older edit up. The person, whom the older edit
// named, takes part in the comment's thread from it, under its event, beside a mention of them
// asserted under that event, which the census of user_threads reads as the participation's cause
// (mentioned_alone); that mention does not stand, its newest tuple a retraction under the same
// event. The role's mention is as this build's write left it - standing from the comment's create
// after the edit, and retracted by the delete - and the session's route stands. Its control is the
// catch-up reading what the comment names now from the older edit's own text rather than the text
// the comment holds (writeOlderEdit's `current` read as `written`): the role's mention is retracted
// under the older edit's event although this build's edit named it, and the person's mention stands
// after this build's delete.
func TestAnOlderBuildsEditCaughtUpLateLeavesTheMentionsALaterWriteLeft(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "LATE", "A late catch-up", "before")
	ctx := context.Background()
	const role, routed = "reviewer", "s-routed"
	for _, later := range []struct {
		name, event string
		write       func(t *testing.T, commentID string)
		// roleStands is that the later write leaves the comment naming the role.
		roleStands bool
	}{{
		name: "an edit back to the role", event: "comment.edited", roleStands: true,
		write: func(t *testing.T, commentID string) {
			requireEdit(t, "edit the comment back to the role", editCommentAs(t, handler, commentID,
				"Over to dispatch://role/"+role+" for the copy, as before.", nil, nil))
		},
	}, {
		name: "the author's delete", event: "comment.deleted",
		write: func(t *testing.T, commentID string) { deleteThreadComment(t, handler, commentID, "alice") },
	}} {
		t.Run(later.name, func(t *testing.T) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body":     "The copy is ready.",
				"mentions": []map[string]any{{"target": "session:" + routed}},
			}, "alice")
			// Create treats a role in the body as a delivery route. Add it through an edit
			// to test a mention that the author can remove while the session route stays.
			requireEdit(t, "name the role in the text alone", editCommentAs(t, handler, comment.ID,
				"Over to dispatch://role/"+role+" for the copy.", nil, nil))
			namedRole := writeEventOf(t, database, "comment.edited", comment.ID)
			requireNewestMention(t, database, comment.ID, graphstore.KindRole, role, true, namedRole)
			older := editAsTheReleaseBefore(t, database, comment, "Over to dispatch://person/"+mentionedPerson+" for the copy.")
			var queued bool
			if err := database.Pool.QueryRow(ctx, `select exists (select 1 from events_inbox_owed where event_id = $1)`, older).Scan(&queued); err != nil || !queued {
				t.Fatalf("the queue holds the older build's edit %d: %t (%v), want it queued: this test proves nothing", older, queued, err)
			}

			later.write(t, comment.ID)
			wrote := writeEventOf(t, database, later.event, comment.ID)
			if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
				t.Fatalf("backfill the older build's edit: %v", err)
			}
			if err := database.Pool.QueryRow(ctx, `select exists (select 1 from events_inbox_owed where event_id = $1)`, older).Scan(&queued); err != nil || queued {
				t.Fatalf("the queue still holds the older build's edit %d after the backfill: %t (%v)", older, queued, err)
			}
			requireNewestMention(t, database, comment.ID, graphstore.KindPerson, mentionedPerson, false, older)
			var asserted int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from graph_tuples
				where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and asserted and source_event = $6
			`, graphstore.KindComment, comment.ID, graphstore.TupleMentions, graphstore.KindPerson, mentionedPerson, older).Scan(&asserted); err != nil || asserted != 1 {
				t.Errorf("the older edit's event holds %d asserted mention of the person (%v), want 1, the cause of their part in the thread", asserted, err)
			}
			requireNewestRelation(t, database, "the person's part in the thread, from the older edit", true, older,
				graphstore.KindPerson, mentionedPerson, graphstore.TupleParticipatesIn, graphstore.KindComment, comment.ID)
			if later.roleStands {
				requireNewestMention(t, database, comment.ID, graphstore.KindRole, role, true, namedRole)
			} else {
				requireNewestMention(t, database, comment.ID, graphstore.KindRole, role, false, wrote)
			}
			requireRelationStands(t, database, "the route after the catch-up",
				graphstore.KindComment, comment.ID, graphstore.TupleMentions, graphstore.KindSession, routed)
		})
	}
}

// commentAsTheReleaseBefore writes alice's comment on issueKey as a build of the release before
// this one writes it, on a connection that carries none of this build's startup parameters
// (olderBuild): under the issue's owner lock, the comment's row with body, the comment_mentions
// row of its delivery route to route, and its comment.created, whose payload is the comment as it
// was written; nothing of the store, and no block of its text. It answers the comment's id and its
// event's.
func commentAsTheReleaseBefore(t *testing.T, database *store.Store, issueKey, body, route string) (string, int64) {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	writer := model.Actor{Kind: "user", ID: "alice"}
	var id string
	var created int64
	if err := pgx.BeginFunc(ctx, olderBuild(t, database), func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		owners.Add(issueKey, "")
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		author, err := json.Marshal(writer)
		if err != nil {
			return err
		}
		var at time.Time
		if err := tx.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ($1, $2::jsonb, $3) returning id::text, created_at
		`, issueKey, author, body).Scan(&id, &at); err != nil {
			return err
		}
		mention := model.Mention{Target: route, Delivery: "btw"}
		if _, err := tx.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery) values ($1::uuid, $2, $3)
		`, id, mention.Target, mention.Delivery); err != nil {
			return err
		}
		created, err = appendAsTheReleaseBefore(ctx, tx, issueKey, "comment.created", writer, model.CommentEventPayload{Comment: model.Comment{
			ID: id, IssueKey: &issueKey, Author: writer, Body: body, CreatedAt: at, Mentions: []model.Mention{mention},
		}})
		return err
	}); err != nil {
		t.Fatalf("write alice's comment as the release before writes it: %v", err)
	}
	return id, created
}

// editAsTheReleaseBefore edits comment to body as a build of the release before this one edits it,
// on an older build's connection (olderBuild): under the issue's owner lock, the row's body and
// edited_at, and the comment.edited that build appends, whose payload is the comment as the edit
// left it; nothing of the store, so neither a mention its new text names nor the retraction of one
// it no longer names. The trigger of migration 20261122100123 queues the event, whose id it answers.
func editAsTheReleaseBefore(t *testing.T, database *store.Store, comment model.Comment, body string) int64 {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	var edited int64
	if err := pgx.BeginFunc(ctx, olderBuild(t, database), func(tx pgx.Tx) error {
		owners := events.OwnerRows{}
		owners.Add(*comment.IssueKey, "")
		if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
			return err
		}
		var editedAt time.Time
		if err := tx.QueryRow(ctx, `
			update comments set body = $2, edited_at = now() where id = $1::uuid returning edited_at
		`, comment.ID, body).Scan(&editedAt); err != nil {
			return err
		}
		comment.Body, comment.EditedAt = body, timestampPtr(&editedAt)
		var err error
		edited, err = appendAsTheReleaseBefore(ctx, tx, *comment.IssueKey, "comment.edited", comment.Author, model.CommentEventPayload{Comment: comment})
		return err
	}); err != nil {
		t.Fatalf("edit comment %s as the release before edits it: %v", comment.ID, err)
	}
	return edited
}

// appendAsTheReleaseBefore appends in tx the event of eventType on issueKey, made by actor, with
// payload, as a build of the release before this one appends it: at the issue's next seq, read
// under the issue's owner lock tx holds. On an older build's connection the trigger of migration
// 20261122100123 queues it where its type is one the backfill catches up.
func appendAsTheReleaseBefore(ctx context.Context, tx pgx.Tx, issueKey, eventType string, actor model.Actor, payload any) (int64, error) {
	event, err := events.NewBroker().Append(ctx, tx, issueOwner(issueKey).event(eventType, actor, payload))
	return event.ID, err
}

// newestRelation is whether the newest tuple of the relation fromKind/fromID -kind-> toKind/toID
// asserts it, and the event it was stated under; false and the sentinel where the store holds none.
func newestRelation(t *testing.T, database *store.Store, fromKind, fromID, kind, toKind, toID string) (bool, int64) {
	t.Helper()
	var asserted bool
	var event int64
	err := database.Pool.QueryRow(context.Background(), `
		select asserted, source_event from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		order by seq desc limit 1
	`, fromKind, fromID, kind, toKind, toID).Scan(&asserted, &event)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, graphstore.NoEvent
	}
	if err != nil {
		t.Fatalf("read %s/%s -%s-> %s/%s: %v", fromKind, fromID, kind, toKind, toID, err)
	}
	return asserted, event
}

// requireNewestRelation fails unless the newest tuple of the relation asserts it as asserted says,
// under event.
func requireNewestRelation(t *testing.T, database *store.Store, what string, asserted bool, event int64, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if stands, under := newestRelation(t, database, fromKind, fromID, kind, toKind, toID); stands != asserted || under != event {
		t.Errorf("%s: the newest %s/%s -%s-> %s/%s asserts %t under event %d, want %t under %d",
			what, fromKind, fromID, kind, toKind, toID, stands, under, asserted, event)
	}
}

// requireRelationStands fails unless the newest tuple of the relation asserts it, under whichever
// event.
func requireRelationStands(t *testing.T, database *store.Store, what, fromKind, fromID, kind, toKind, toID string) {
	t.Helper()
	if stands, under := newestRelation(t, database, fromKind, fromID, kind, toKind, toID); !stands {
		t.Errorf("%s: %s/%s -%s-> %s/%s does not stand (its newest tuple is under event %d), want it standing",
			what, fromKind, fromID, kind, toKind, toID, under)
	}
}

// R5's mention of a person is an edge of References as the comment's other edges are, and a
// poller reads it as it reads them: the edge's source_seq is the event of the write that stated it
// (graph_edges' identity arm answers the tuple's legacy_source_seq, which refs' stateMention and
// graphstore.QueueCaused give it), so a ?since= read of the comment whose window holds that event
// lists the mention, and a read since that event itself lists it no more. awaitSinceRead settles
// which answer of the read after the write is final.
func TestASinceReadListsTheIdentityMentionTheWriteInItsWindowStated(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SINC", "Mentions since", "before")
	// The log's head is a safe since here, unlike for a live poller: the comment draws its event
	// id after this read, so that id stands above every id the read could see.
	beforeTheComment := eventHead(t, database)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Over to dispatch://person/" + mentionedPerson + " for the copy.",
	}, "alice")
	created := writeEventOf(t, database, "comment.created", comment.ID)
	commentRef := "dispatch://" + issue.Key + "/comment/" + comment.ID

	since := url.Values{"from": {commentRef}, "since": {strconv.FormatInt(beforeTheComment, 10)}}
	awaitSinceRead(t, database, "?from= the comment since the log's head before it, wanting its mention of "+mentionedPerson+
		" stamped with event "+strconv.FormatInt(created, 10),
		func() model.GraphReferences { return graphEdges(t, handler, since) },
		func(read model.GraphReferences) bool {
			return slices.ContainsFunc(read.Edges, func(edge model.GraphEdge) bool {
				return edge.Kind == graphstore.TupleMentions && edge.Node.Kind == graphstore.KindPerson && edge.Node.ID == mentionedPerson &&
					edge.SourceSeq != nil && *edge.SourceSeq == created
			})
		})
	// The awaited read listed the mention, so the horizon had passed the comment's event; the
	// horizon only grows, so a read since that same event answers no mention because of the since
	// filter and not because of a stale horizon.
	if edges := graphEdges(t, handler, url.Values{"from": {commentRef}, "since": {strconv.FormatInt(created, 10)}}).Edges; slices.ContainsFunc(edges,
		func(edge model.GraphEdge) bool { return edge.Node.Kind == graphstore.KindPerson }) {
		t.Fatalf("?from= the comment since its own event lists %#v, want no mention of a person", edges)
	}
}

// R5's delivery route is a mention as a name in the comment's text is: a session the request's
// mentions array names is a mentions tuple out of the comment (graphstore.CommentMentionFacts),
// and a poller reads it as it reads the comment's other edges. Its tuple carries the comment's
// event as its legacy_source_seq, which graph_edges' identity arm answers as the edge's source_seq,
// so a ?since= read of the comment whose window holds that event lists the session, and a read
// since that event itself lists it no more. The comment's body names nobody, so only the array
// names the session. A read lists an edge only to a node it resolves, and no write of this test
// reaches the session - the server has no listener to send the mention through - so the test
// writes the session's node first, as the write that first reached it would have.
func TestASinceReadListsTheRouteOnlyTheRequestsMentionsName(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ROUT", "Routes since", "before")
	const session = "s-routed"
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of session %s's node: %v", session, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		graphstore.SessionNode(session, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent),
	}, nil); err != nil {
		t.Fatalf("write session %s's node: %v", session, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit session %s's node: %v", session, err)
	}
	// The log's head is a safe since here, as in the test above: the comment draws its event id
	// after this read.
	beforeTheComment := eventHead(t, database)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Can you take the copy?", "mentions": []map[string]any{{"target": "session:" + session}},
	}, "alice")
	created := writeEventOf(t, database, "comment.created", comment.ID)
	commentRef := "dispatch://" + issue.Key + "/comment/" + comment.ID
	routed := func(edge model.GraphEdge) bool {
		return edge.Kind == graphstore.TupleMentions && edge.Node.Kind == graphstore.KindSession && edge.Node.ID == session
	}

	since := url.Values{"from": {commentRef}, "since": {strconv.FormatInt(beforeTheComment, 10)}}
	awaitSinceRead(t, database, "?from= the comment since the log's head before it, wanting its route to "+session+
		" stamped with event "+strconv.FormatInt(created, 10),
		func() model.GraphReferences { return graphEdges(t, handler, since) },
		func(read model.GraphReferences) bool {
			return slices.ContainsFunc(read.Edges, func(edge model.GraphEdge) bool {
				return routed(edge) && edge.SourceSeq != nil && *edge.SourceSeq == created
			})
		})
	// The awaited read listed the route, so the horizon had passed the comment's event, and a read
	// since that same event answers no route because of the since filter alone.
	if edges := graphEdges(t, handler, url.Values{"from": {commentRef}, "since": {strconv.FormatInt(created, 10)}}).Edges; slices.ContainsFunc(edges, routed) {
		t.Fatalf("?from= the comment since its own event lists %#v, want no route to session %s", edges, session)
	}
}
