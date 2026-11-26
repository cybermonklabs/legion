package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

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
