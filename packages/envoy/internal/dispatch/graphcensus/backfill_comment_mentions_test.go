package graphcensus

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// A comment an older build wrote naming three delivery routes - a role, which resolved to a
// session, a session, and a session past the store's bound - each a comment_mentions row holding
// no tuples_stored (migration 20261120090000), written in the comment's own statement before this
// release's schema reached the database. Before the backfill the census checks none of them,
// counts the two within the bound unmarked and the third past the bound. The backfill states what
// this release's comment write states of them (api's commentMentionFacts): a mentions tuple to the
// role, one to the session it resolved to and one to the session named, and the comment's delivery
// once, each under the comment.created that stored the comment, the event its node is stated
// under, and at the comment's created_at; it marks the two rows and leaves the third, which states
// no tuple, to its false. After it the census checks the two as the release before's, with no row
// unmarked and no difference, and a second run appends nothing. Its control is the mention pass
// taken out of the backfill (inboxPasses): the two rows stay unmarked.
func TestTheBackfillStatesTheDeliveryRoutesAnOlderBuildsCommentNames(t *testing.T) {
	f := newInboxCensus(t)
	writes := f.writes
	pastBound := "session:" + strings.Repeat("s", graphstore.MaxAddressBytes+1)
	comment := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		with written as (
			insert into comments (issue_key, author, body, created_at)
			values ('CENS-1', $1, 'Can the role and the session take this?', '2020-01-01T00:00:00Z')
			returning id, created_at
		), mentioned as (
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			select written.id, route.target, 'btw', route.session
			from written
			cross join (values ('role:mention-role'::text, 'ses-role'::text), ('session:ses-named', 'ses-named'), ($2::text, null::text))
				as route (target, session)
		)
		select id::text, created_at from written`, actorJSON(t, alice), pastBound)

	report, _ := f.census(t, "before the backfill")
	if kind := censusKind(t, report, "comment_mentions"); kind.Unmarked != 2 || kind.Rows != 0 || kind.PastBound != 1 {
		t.Errorf("before the backfill comment_mentions reads %d unmarked row(s), %d checked and %d past the bound, want 2, none and 1",
			kind.Unmarked, kind.Rows, kind.PastBound)
	}

	backfilled := backfillInboxOnce(t, writes.database)
	report, _ = f.census(t, "after the backfill")
	kind := censusKind(t, report, "comment_mentions")
	if kind.Rows == 0 || kind.RowDifferences != 0 || kind.NodeDifferences != 0 || kind.Unmarked != 0 || kind.PastBound != 1 {
		t.Errorf("after the backfill comment_mentions reads %d row(s), %d rows no tuple states, %d tuples no row projects (%v %v), "+
			"%d unmarked and %d past the bound, want rows, no difference, none unmarked and 1 past the bound",
			kind.Rows, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes, kind.Unmarked, kind.PastBound)
	}
	if !kind.CountsPopulations || kind.EarlierRelease != 2 || kind.ThisRelease != 0 {
		t.Errorf("after the backfill comment_mentions reads %d row(s) of the release before and %d of this one (populations counted: %t), want 2 and 0",
			kind.EarlierRelease, kind.ThisRelease, kind.CountsPopulations)
	}
	node := graphstore.KindComment + ":" + comment.id
	requireTuples(t, writes.database, "the delivery routes of alice's comment", []string{graphstore.TupleMentions, graphstore.TupleDelivery},
		[]string{node},
		innerTuple{from: node, kind: graphstore.TupleMentions, to: graphstore.KindRole + ":mention-role", event: comment.event, at: comment.at},
		innerTuple{from: node, kind: graphstore.TupleMentions, to: graphstore.KindSession + ":ses-role", event: comment.event, at: comment.at},
		innerTuple{from: node, kind: graphstore.TupleMentions, to: graphstore.KindSession + ":ses-named", event: comment.event, at: comment.at},
		innerTuple{from: node, kind: graphstore.TupleDelivery, to: graphstore.KindValue + ":btw", event: comment.event, at: comment.at},
	)
	if done := reportedKind(t, backfilled, "comment_mentions"); done.Rows != 2 {
		t.Errorf("the comment_mentions pass reports %#v, want 2 rows", done)
	}
	requireASecondRunAppendsNothing(t, writes.database)
}
