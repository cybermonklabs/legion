package access

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestDocumentClauseAnswersAsItemOwnerClauseOverEveryDocument: DocumentClause, which reads a
// document's own row, answers every seeded viewer exactly as ItemOwnerClause does over the same
// document by its issue and its id - an issue's spec in PRIV, a document of PUB, and a file of an
// agent's conversation whose session acted in PRIV - since a document's project_key is its issue's
// project, written with the row and never changed after. And it answers the rule: bob, bob's
// token and carol's token read PRIV's spec, alice does not; alice and carol's token, a full
// member's, whose list decides only which private projects it reads, read PUB's document, bob and
// his token do not; the conversation's file, whose session acted in PRIV, is read by carol's
// token, an open viewer that sees PRIV, and by none of the others, its session's own conversation,
// and by the server, which reads every one.
func TestDocumentClauseAnswersAsItemOwnerClauseOverEveryDocument(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into session_projects_filled default values;
		insert into session_projects (session_id, project_key) values ('ses_priv', 'PRIV');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"olga@example.com"}', 'U');
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('PRIV-1', 'PRIV', 'spec', 'spec.md', 'doc', '{"kind":"user","id":"olga@example.com"}');
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('PUB', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"olga@example.com"}');
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('ses_priv', 'plan', 'plan.md', 'doc', '{"kind":"session","id":"ses_priv"}');
	`); err != nil {
		t.Fatalf("seed the documents: %v", err)
	}
	callers := map[string]model.Actor{
		"alice": cookie(alice), "bob": cookie(bob), "carol's token naming PRIV": personalToken(carol, "PRIV"),
		"bob's token naming PRIV": personalToken(bob, "PRIV"),
	}
	viewers := map[string]model.Viewer{"the server": *model.ServerViewer}
	for name, actor := range callers {
		viewer, err := Resolve(ctx, database.Pool, actor)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		viewers[name] = viewer
	}
	want := map[string]map[string]bool{
		"spec":  {"alice": false, "bob": true, "carol's token naming PRIV": true, "bob's token naming PRIV": true, "the server": true},
		"notes": {"alice": true, "bob": false, "carol's token naming PRIV": true, "bob's token naming PRIV": false, "the server": true},
		"plan":  {"alice": false, "bob": false, "carol's token naming PRIV": true, "bob's token naming PRIV": false, "the server": true},
	}
	compared := 0
	for name, viewer := range viewers {
		rows, err := database.Pool.Query(ctx, `
			select a.slug, `+DocumentClause("a", "$1")+`, `+ItemOwnerClause("a.issue_key", "a.id", "$1")+`
			from artifacts a order by a.slug`, viewerArg(t, viewer))
		if err != nil {
			t.Fatalf("%s: read the documents: %v", name, err)
		}
		for rows.Next() {
			var refKey string
			var document, owner bool
			if err := rows.Scan(&refKey, &document, &owner); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			compared++
			if document != owner {
				t.Errorf("%s reads %s: DocumentClause %t, ItemOwnerClause %t", name, refKey, document, owner)
			}
			if expected, ok := want[refKey][name]; !ok || document != expected {
				t.Errorf("%s reads %s: DocumentClause %t, want %t (known %t)", name, refKey, document, expected, ok)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: read the documents: %v", name, err)
		}
	}
	if compared != len(viewers)*len(want) {
		t.Fatalf("compared %d documents, want %d: the seed's documents were not all read", compared, len(viewers)*len(want))
	}
}
