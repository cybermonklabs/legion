package docs

import (
	"context"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The delivery routes the comment of this file's test names: a role, which resolved to a session,
// and a session named directly.
const (
	mentionedRole        = "previous-release-mentioned-role"
	mentionedRoleSession = "previous-release-mentioned-role-session"
	mentionedSession     = "previous-release-mentioned-session"
)

// The delivery routes a comment names that the release before this one writes after the backfill
// reach the store at this build's next block-index tick, with no second backfill, and no
// comment_mentions row stays unmarked: R9 says of the unmarked class "That class reaches zero when
// the backfill ends ... No class is permanent."
//
// That release writes a comment's comment_mentions rows in the comment's own transaction, beside
// the comment.created that stores it, each holding no tuples_stored (migration 20261120090000), and
// states nothing of them in the store. The tick that reads that event gives the comment its node
// and then, in the same transaction, the backfill's own mention pass over the comment's rows
// (graphcensus.CatchUpComments): the mentions tuples of the role, of the session it resolved to and
// of the session named, and the comment's delivery, under that event, and each row's tuples_stored.
//
// The issue is routed to the role before the store exists, so the backfill gives the role its node,
// as this build's outbox gives one to a role an event names. The stand-in for that release writes,
// after the backfill and while it holds every period of the lease, bob's comment naming the role and
// the session. Once it leaves, this build's next tick must leave both rows marked, the census
// checking comment_mentions with no row unmarked and no difference, and the mention pass owing
// nothing. Its control is the mention pass taken out of the tick's catch-up (CatchUpComments and
// CatchUpOwedRows): both rows stay owed.
func TestTheDeliveryRoutesOfACommentThePreviousReleaseWritesAfterTheBackfillReachTheStore(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	appendIssueEvent(t, database)
	const slug = "mentioned-beside"
	document := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
	settle(t, database, document)
	// Rows of every other kind the census reads, so the census can pass only on agreement.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-mentioned-author"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:`+mentionedRole+`' where key = 'CORE-1';
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

	// A process of the release before joins, takes every period of the lease, writes bob's comment,
	// and leaves.
	registerProcess(t, database, "")
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	comment := writeBesideAsThePreviousRelease(t, database, model.Actor{Kind: "user", ID: "bob"}, "", "",
		"Can the role and the session take this?", false,
		model.Mention{Target: "role:" + mentionedRole, Delivery: "btw", SessionID: new(mentionedRoleSession)},
		model.Mention{Target: "session:" + mentionedSession, Delivery: "btw", SessionID: new(mentionedSession)})
	routes := []inboxClass{{
		name: "the comment_mentions rows of bob's comment",
		owed: `select count(*) from comment_mentions where comment_id = $1::uuid and not tuples_stored`,
		args: []any{comment},
	}}
	requireInboxClassesOwed(t, database, routes, true, "after the previous release wrote bob's comment, before this build's tick")
	previousLeaves()

	// This build's next tick, serving alone again.
	tick := awaitedBlockIndexTick(t, service)
	t.Logf("the first tick after the previous release left read %d event(s), gave %d comment(s) their nodes, marked %d inbox row(s), another build %t",
		tick.Events, tick.Comments, tick.InboxRows, tick.AnotherBuild)
	requireInboxClassesOwed(t, database, routes, false, "after the first tick after the previous release left, with no second backfill")
	requireNoInboxRowOwed(t, database, "after the first tick after the previous release left")
	checked := false
	for _, kind := range takeStoreCensus(t, database).Kinds {
		if kind.Kind != "comment_mentions" {
			continue
		}
		checked = true
		if kind.Rows == 0 || kind.Unmarked != 0 {
			t.Errorf("the census checks %d row(s) of comment_mentions and counts %d unmarked after the first tick after the previous release left, want rows and none unmarked",
				kind.Rows, kind.Unmarked)
		}
	}
	if !checked {
		t.Fatal("the census reads no kind comment_mentions")
	}
	requireCensusReadsZero(t, database, "after the previous release wrote a comment naming a role and a session and left, with no second backfill")
}
