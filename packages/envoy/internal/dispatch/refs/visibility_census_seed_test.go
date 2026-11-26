package refs_test

import (
	"context"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func init() { refs.SeedStoreThroughItsWriters = seedStoreThroughItsWriters }

// seedStoreThroughItsWriters writes the rows that address the people and agents of a running
// deployment - a comment a person wrote, a comment a session wrote and an issue routed to a
// role - and then runs the writer that states the store from such rows, graphcensus's backfill:
// the person, the session, the role and the role's page are each a node that writer makes from
// one of those rows, as it makes them for a database written before the store existed. No kind
// is named here; which kinds the store then holds is what the census reads from graph_nodes.
func seedStoreThroughItsWriters(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('CENSUS', 'Census project', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank, route)
		 values ('CENSUS-1', 'CENSUS', 1, 'Census issue', '{"kind":"user","id":"alice"}', 'U', 'role:census-role')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"user","id":"alice"}', 'A person''s remark.')`,
		`insert into comments (issue_key, author, body)
		 values ('CENSUS-1', '{"kind":"session","id":"census-session"}', 'A session''s remark.')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the rows that address people and agents: %v", err)
		}
	}
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("state the store from those rows through its backfill: %v", err)
	}
}
