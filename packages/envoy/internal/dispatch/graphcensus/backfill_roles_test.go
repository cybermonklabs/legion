package graphcensus

import (
	"context"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The backfill gives every role the store census holds to a node its node, its page and the
// page's part_of the role, so that a database written before the publisher wrote them reads
// zero differences once it has run. The census holds a role to a node wherever a row addresses
// it now or an event of the log ever addressed it (addressedRoles), and the publisher writes one
// only for the events it publishes from its own release on (outbox.deliveredGraph), so every role
// an older database names is owed one.
//
// The rows and events are written as the API's handlers write them (addressingWrites) and no
// outbox runs, which is what an older database holds: an issue routed to one role and then to
// another, a message targeted at a role and a comment mentioning one, each with the event its
// write appended, and an issue row routed to a role no event names. The census before the run
// names each of those roles as a row with no node. After the run it reads zero differences, and
// each role's node and page are the run's, caused by the first event that named the role, or by
// the sentinel for the role only a row names. A second run writes nothing.
func TestTheBackfillGivesEveryRoleTheCensusNamesItsNodeAndItsPage(t *testing.T) {
	const (
		routedFirst = "backfill-route-first"
		routedNow   = "backfill-route-now"
		messaged    = "backfill-message-target"
		mentioned   = "backfill-comment-mention"
		rowOnly     = "backfill-row-only"
	)
	ctx := context.Background()
	writes := newAddressingWrites(t)
	alice := model.Actor{Kind: "user", ID: "alice"}

	writes.createIssue(t, "CENS-1", 1, "U", alice)
	first := writes.route(t, "CENS-1", routedFirst, alice)
	now := writes.route(t, "CENS-1", routedNow, alice)
	message := writes.targetMessage(t, "CENS-1", messaged, alice)
	_, comment := writes.mentionRole(t, "CENS-1", mentioned, "ses_backfill_mention_reader", alice)
	if _, err := writes.database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('CENS-2', 'CENS', 2, 'Routed by a row alone', $1, '{"kind":"user","id":"alice"}', 'V')
	`, "role:"+rowOnly); err != nil {
		t.Fatalf("write the issue routed by a row alone: %v", err)
	}
	causes := map[string]int64{
		routedFirst: first.ID, routedNow: now.ID, messaged: message.ID, mentioned: comment.ID,
		rowOnly: graphstore.NoEvent,
	}

	before := storeCensusOf(t, writes.database)
	named := map[string]string{}
	for _, difference := range censusKind(t, before, graphstore.KindRole).ReportedRows {
		named[difference.At] = difference.Cause
	}
	for role := range causes {
		if cause := named["role "+role]; cause != "the row names a node the store does not hold" {
			t.Fatalf("the census before the backfill reads role %s as %q, want a row with no node: the fixture holds what an older database holds",
				role, cause)
		}
	}

	report, err := Backfill(ctx, writes.database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	requireRoleHistoryOnly(t, writes.database, routedFirst)
	requireTheRoleCensusAgrees(t, writes.database, routedFirst, routedNow, messaged, mentioned, rowOnly)
	for role, cause := range causes {
		requireBackfilledRole(t, writes.database, role, cause)
	}
	done := backfilledRoles(t, report)
	if done.Rows != int64(len(causes)) || done.Nodes != 2*int64(len(causes)) || done.Remaining != 0 || !done.Complete {
		t.Fatalf("the backfill reports roles %+v, want %d roles given %d nodes and none remaining",
			done, len(causes), 2*len(causes))
	}

	nodes, tuples := storeSize(t, writes.database)
	again, err := Backfill(ctx, writes.database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store again: %v", err)
	}
	if done := backfilledRoles(t, again); done.Rows != 0 || done.Nodes != 0 {
		t.Fatalf("a second backfill reports roles %+v, want nothing written", done)
	}
	if againNodes, againTuples := storeSize(t, writes.database); againNodes != nodes || againTuples != tuples {
		t.Fatalf("a second backfill took the store from %d nodes and %d tuples to %d and %d, want no change",
			nodes, tuples, againNodes, againTuples)
	}
}

// backfilledRoles is the role kind's line of a backfill report.
func backfilledRoles(t *testing.T, report BackfillReport) KindBackfill {
	t.Helper()
	for _, done := range report.Kinds {
		if done.Kind == graphstore.KindRole {
			return done
		}
	}
	t.Fatalf("the backfill reports no %s kind: %#v", graphstore.KindRole, report)
	return KindBackfill{}
}

// requireBackfilledRole fails unless role's node, its page and the page's one part_of the role
// were written by the backfill and caused by cause.
func requireBackfilledRole(t *testing.T, database *store.Store, role string, cause int64) {
	t.Helper()
	ctx := context.Background()
	page := graphstore.RolePageID(role)
	for _, node := range []struct{ kind, id string }{{graphstore.KindRole, role}, {graphstore.KindPage, page}} {
		held, found, err := graphstore.ReadNode(ctx, database.Pool, node.kind, node.id)
		if err != nil || !found {
			t.Fatalf("read node %s %s: found %t, %v", node.kind, node.id, found, err)
		}
		if held.SourceEvent != cause || !held.MadeBy.SameAs(backfillActor) {
			t.Fatalf("node %s %s was made by %+v under event %d, want the backfill under event %d",
				node.kind, node.id, held.MadeBy, held.SourceEvent, cause)
		}
	}
	parts, err := graphstore.History(ctx, database.Pool, graphstore.KindPage, page, graphstore.TuplePartOf)
	if err != nil {
		t.Fatalf("read the part_of history of %s: %v", page, err)
	}
	if len(parts) != 1 || parts[0].ToKind != graphstore.KindRole || parts[0].ToID != role || !parts[0].Asserted ||
		parts[0].SourceEvent != cause || !parts[0].Who.SameAs(backfillActor) {
		t.Fatalf("the page %s hangs under %+v, want the role %s alone, by the backfill under event %d", page, parts, role, cause)
	}
}

// storeSize counts every node and every tuple the store holds.
func storeSize(t *testing.T, database *store.Store) (nodes, tuples int64) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_nodes), (select count(*) from graph_tuples)
	`).Scan(&nodes, &tuples); err != nil {
		t.Fatalf("count the store's rows: %v", err)
	}
	return nodes, tuples
}
