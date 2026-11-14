package store

import (
	"context"
	"testing"
)

// openLabelTestStore is a migrated database of its own holding one project and one issue, with
// the fill recorded finished: the state a deployment is in once the labelling is over and a
// project-scoped meaning search reads embeddings.project_key alone (api/search.go,
// meaningProjectScope).
func openLabelTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, statement := range []struct {
		why string
		sql string
	}{
		{"seed the project", `insert into projects (key, name, default_assignee) values ('LABL', 'Label', 'alice@d.example')`},
		{"seed the issue", `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('LABL-1', 'LABL', 1, 'An issue to label', '{"kind":"user","id":"alice"}', '00000001')`},
		{"record the fill finished", `update embeddings_project_key_backfill set done = true`},
		{"take away the row the issue's trigger wrote", `delete from embeddings where kind = 'issue' and id = 'LABL-1'`},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql); err != nil {
			t.Fatalf("%s: %v", statement.why, err)
		}
	}
	return database
}

// projectKeyOf is the project a row carries, and whether it carries one at all.
func projectKeyOf(t *testing.T, database *Store, kind, id string) (string, bool) {
	t.Helper()
	var key *string
	if err := database.Pool.QueryRow(context.Background(),
		"select project_key from embeddings where kind = $1 and id = $2", kind, id).Scan(&key); err != nil {
		t.Fatalf("read the project of %s/%s: %v", kind, id, err)
	}
	if key == nil {
		return "", false
	}
	return *key, true
}

// The one writer that can insert an embeddings row carrying no project is the release before
// this one: it serves beside this one for the whole of a roll's drain and for as long as a
// rollback to it lasts, and its coverage insert is its own Go rather than one of the two enqueue
// functions this database has replaced. At 0bf41f394392caa9451ab8d6973e14d4d98c8e1d
// (embedqueue/coverage.go, insertMissingIssues) that statement names
// (kind, id, text_snapshot, content_hash) and no project_key, so the row it adds for an issue
// that has none is inserted null - and a null written once the fill has recorded itself done is
// a row nothing will ever label, since the fill is over and no later write touches a row whose
// text does not change.
//
// So the rule sits in the database (migration 20261110064913): a row inserted with no project
// takes its owner's. The statement below is that release's, word for word but for the issue it
// names, and the label is read back from the row it wrote.
func TestARowInsertedAsTheReleaseBeforeThisOneInsertsItCarriesItsProject(t *testing.T) {
	database := openLabelTestStore(t)
	ctx := context.Background()

	if _, err := database.Pool.Exec(ctx, `
		insert into embeddings (kind, id, text_snapshot, content_hash)
		select $1::text, i.key, i.title, encode(sha256(convert_to(i.title, 'UTF8')), 'hex')
		from issues i
		where i.key = 'LABL-1'
		  and not exists (select 1 from embeddings e where e.kind = $1::text and e.id = i.key)
		on conflict (kind, id) do nothing
	`, "issue"); err != nil {
		t.Fatalf("insert the coverage row as the release before this one inserts it: %v", err)
	}

	key, labelled := projectKeyOf(t, database, "issue", "LABL-1")
	if !labelled {
		t.Fatal("the row the earlier release's coverage insert wrote carries no project, so no project-scoped meaning search can return it and no later pass will label it")
	}
	if key != "LABL" {
		t.Errorf("the row carries the project %q, want %q: a window labelled with another project's key is as invisible to its own project as a null one", key, "LABL")
	}
}

// A row whose owner resolves to no project keeps a null and the insert stands: the trigger
// answers what embeddings_owner_project_key answers, and that is null for a comment that was
// never stored, for an id in no shape a uuid cast takes, and for the retired whole-text kinds.
// No rule can give such a row a project, and no meaning arm would have returned it either
// (20261109120000), so the insert must not fail over it.
func TestARowWhoseOwnerIsGoneIsInsertedWithNoProject(t *testing.T) {
	database := openLabelTestStore(t)
	ctx := context.Background()

	for _, row := range []struct {
		what string
		kind string
		id   string
	}{
		{"a window of a comment that was never stored", "comment_block", "comment/9c8d4b9e-0f3a-4a2f-8f1d-5a7c2e0b6d31#b1"},
		{"a window whose owner id is no uuid", "comment_block", "comment/not-a-uuid#b1"},
		{"a retired whole-text row", "comment", "9c8d4b9e-0f3a-4a2f-8f1d-5a7c2e0b6d31"},
	} {
		if _, err := database.Pool.Exec(ctx, `
			insert into embeddings (kind, id, text_snapshot, content_hash) values ($1, $2, 'some text', $2)
		`, row.kind, row.id); err != nil {
			t.Fatalf("insert %s: %v", row.what, err)
		}
		if key, labelled := projectKeyOf(t, database, row.kind, row.id); labelled {
			t.Errorf("%s carries the project %q, want none: its owner resolves to no project", row.what, key)
		}
	}
}
