package graphcensus

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A build before this one writes no node, so a document it removes leaves this build's node live
// behind no row, and a document it makes next at the same issue and name takes that address and
// stands behind that node. The node's live statement was made for the first row, and the census
// names the second row gone, whatever the two rows' times: the statement names the row it was made
// for by that row's own create event, the artifact.created whose payload names the row, and a
// statement at the sentinel names a row only when the row has no create event of its own. Here the
// second row's created_at is forced to equal the time of the first row's live statement, which is
// the one case where a node's time could not tell the rows apart; both variants are run, the first
// row's statement under its own event and at the sentinel (a document made before Dispatch recorded
// artifact.created). In each, the census names the row gone, and the backfill states the node gone
// and then live under the second row's own event, and moves the node's row onto that event: its
// own row moved no base attribute, but its writer stated the node's state itself, so the node
// write marks it for the append to move (graphstore's queueNodes). After that the census names
// nothing.
//
// Each variant also reads the rule by time alone over the same rows, and holds it to reading the
// second row as the node's own: the case is the collision a time cannot see, so the event is what
// tells it. Its control is ownLiveStatement read by time (the newest state statement dated at the
// row's created_at): the census then names nothing, and the test fails.
func TestADocumentAtAnOrphanedNodesAddressIsNamedGoneWhateverItsTime(t *testing.T) {
	for _, variant := range []struct {
		name  string
		event bool
	}{{"the first row's statement under its create event", true}, {"the first row's statement at the sentinel", false}} {
		t.Run(variant.name, func(t *testing.T) {
			database := storetest.Open(t)
			ctx := context.Background()
			const ref = "IDEN-1/notes-md"
			at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
			alice := model.Actor{Kind: "user", ID: "alice"}
			seedIdentityIssue(t, database, at, alice)
			first := insertIdentityDocument(t, database, "alice", at)
			firstEvent := graphstore.NoEvent
			if variant.event {
				firstEvent = insertCreateEvent(t, database, 1, first)
			}
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				made, err := graphstore.WriteUncaused(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(ref, "IDEN", "", alice, at)}, nil)
				if err != nil {
					return err
				}
				return graphstore.Caused(ctx, tx, made, firstEvent)
			}); err != nil {
				t.Fatalf("write the first document's node: %v", err)
			}
			// A build before this one removes the first document and makes the second at its
			// address, dated at the very time of the first row's live statement.
			if _, err := database.Pool.Exec(ctx, `delete from artifacts where id = $1`, first); err != nil {
				t.Fatalf("remove the first document as a build before this one does: %v", err)
			}
			second := insertIdentityDocument(t, database, "carol", at)
			secondEvent := insertCreateEvent(t, database, 2, second)

			var byTime bool
			if err := database.Pool.QueryRow(ctx, `
				select (select t.at from graph_tuples t
				        where t.from_kind = 'artifact' and t.from_id = $1 and t.kind = 'state' and t.to_kind = 'value'
				        order by t.seq desc limit 1) = (select created_at from artifacts where ref_key = $1)
			`, ref).Scan(&byTime); err != nil {
				t.Fatalf("read the rule by time: %v", err)
			}
			if !byTime {
				t.Fatalf("the node's newest state statement is not dated at the second row's created_at, so this is not the collision a time cannot see")
			}
			incomplete, err := ProjectComplete(ctx, database.Pool)
			if err != nil {
				t.Fatalf("read the incomplete rows: %v", err)
			}
			if want := []string{"artifact " + ref + ": gone" + backfillCure}; !slices.Equal(incomplete, want) {
				t.Fatalf("ProjectComplete answers %q, want %q: the node's live statement is the first row's", incomplete, want)
			}

			if _, err := Backfill(ctx, database, &bytes.Buffer{}); err != nil {
				t.Fatalf("run the backfill: %v", err)
			}
			requireComplete(t, ctx, database, "after the backfill")
			history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, ref, graphstore.AttributeState)
			if err != nil || len(history) != 3 {
				t.Fatalf("read the state history of %s: %d tuple(s), %v; want live, gone, live", ref, len(history), err)
			}
			if gone, live := history[1], history[2]; gone.ToID != graphstore.StateGone || gone.SourceEvent != secondEvent ||
				live.ToID != graphstore.StateLive || live.SourceEvent != secondEvent || live.Who.ID != "carol" || !live.At.After(at) {
				t.Errorf("the backfill stated %+v then %+v, want gone then live under the second row's event %d, the live one by carol and after %s",
					gone, live, secondEvent, at)
			}
			var source int64
			if err := database.Pool.QueryRow(ctx, `
				select source_event from graph_nodes where kind = $1 and id = $2
			`, graphstore.KindArtifact, ref).Scan(&source); err != nil || source != secondEvent {
				t.Errorf("the node of %s is at event %d (%v) after the backfill, want the second row's event %d, not the removed row's %d",
					ref, source, err, secondEvent, firstEvent)
			}
		})
	}
}

// seedIdentityIssue writes project IDEN and issue IDEN-1 with its node, made at by author.
func seedIdentityIssue(t *testing.T, database *store.Store, at time.Time, author model.Actor) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('IDEN', 'Identity', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('IDEN-1', 'IDEN', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed the project and the issue: %v", err)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{graphstore.IssueNode("IDEN-1", "IDEN", author, at)}, nil)
	}); err != nil {
		t.Fatalf("write the issue's node: %v", err)
	}
}

// insertIdentityDocument writes the document IDEN-1/notes-md, made by author at created, as a
// build before this one writes it: the row alone. It answers the row's id.
func insertIdentityDocument(t *testing.T, database *store.Store, author string, created time.Time) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by, created_at)
		values ('IDEN-1', 'IDEN', 'notes-md', 'Notes', 'doc', jsonb_build_object('kind', 'user', 'id', $1::text), $2)
		returning id::text
	`, author, created).Scan(&id); err != nil {
		t.Fatalf("write %s's document: %v", author, err)
	}
	return id
}

// insertCreateEvent appends, at seq of IDEN-1, the artifact.created event of the document row id,
// whose payload names the row as every release's create names it (api's
// artifactCreatedEventPayload), and answers the event's id.
func insertCreateEvent(t *testing.T, database *store.Store, seq int, id string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ('IDEN-1', $1, 'artifact.created', '{"kind":"user","id":"alice"}', jsonb_build_object('artifact', jsonb_build_object('id', $2::text)), false)
		returning id
	`, seq, id).Scan(&event); err != nil {
		t.Fatalf("append the create event of %s: %v", id, err)
	}
	return event
}
